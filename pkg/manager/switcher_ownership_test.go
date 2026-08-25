package manager

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const (
	migrationSourceProvider = "torbox-primary"
	migrationTargetProvider = "torbox-secondary"
	migrationSourceID       = "17"
	migrationTargetID       = "99"
)

type migrationOwnershipHarness struct {
	manager         *Manager
	entry           *storage.Entry
	sourceDeletes   chan string
	targetDeletes   chan string
	sourceErr       error
	targetSubmitErr error
	targetCheckErr  error
	submitCalls     atomic.Int32
}

func newMigrationOwnershipHarness(t *testing.T) *migrationOwnershipHarness {
	t.Helper()

	store := newLifecycleTestStorage(t)
	harness := &migrationOwnershipHarness{
		sourceDeletes: make(chan string, 4),
		targetDeletes: make(chan string, 4),
	}

	sourceClient := &routingTestClient{
		cfg: config.Debrid{Provider: "torbox", Name: migrationSourceProvider},
		delete: func(id string) error {
			harness.sourceDeletes <- id
			return harness.sourceErr
		},
	}
	targetClient := &routingTestClient{
		cfg: config.Debrid{Provider: "torbox", Name: migrationTargetProvider},
		submit: func(torrent *debridTypes.Torrent) (*debridTypes.Torrent, error) {
			harness.submitCalls.Add(1)
			torrent.Id = migrationTargetID
			torrent.Debrid = migrationTargetProvider
			torrent.Status = debridTypes.TorrentStatusDownloaded
			torrent.Files = map[string]debridTypes.File{
				"movie.mkv": {
					TorrentId: migrationTargetID,
					Id:        "target-file",
					Name:      "movie.mkv",
					Path:      "movie.mkv",
					Link:      "https://target.invalid/movie.mkv",
					Size:      1024,
				},
			}
			return torrent, harness.targetSubmitErr
		},
		check: func(torrent *debridTypes.Torrent) (*debridTypes.Torrent, error) {
			return torrent, harness.targetCheckErr
		},
		delete: func(id string) error {
			harness.targetDeletes <- id
			return nil
		},
	}

	clients := xsync.NewMap[string, debrid.Client]()
	clients.Store(migrationSourceProvider, sourceClient)
	clients.Store(migrationTargetProvider, targetClient)
	cfg := &config.Config{Debrids: []config.Debrid{
		{Provider: "torbox", Name: migrationSourceProvider},
		{Provider: "torbox", Name: migrationTargetProvider},
	}}
	manager := &Manager{
		storage: store,
		clients: clients,
		config:  cfg,
		logger:  zerolog.Nop(),
	}
	manager.entry = NewEntryCache(manager)
	manager.fixer = &Fixer{manager: manager}
	harness.manager = manager
	t.Cleanup(func() {
		manager.stopAcceptingBackgroundWork()
		manager.background.Wait()
	})

	infoHash := "0123456789abcdef0123456789abcdef01234567"
	entry := &storage.Entry{
		Protocol:       config.ProtocolTorrent,
		InfoHash:       infoHash,
		Name:           "movie.mkv",
		Magnet:         "magnet:?xt=urn:btih:" + infoHash + "&dn=movie.mkv",
		ActiveProvider: migrationSourceProvider,
		Providers: map[string]*storage.ProviderEntry{
			migrationSourceProvider: {
				Provider: migrationSourceProvider,
				ID:       migrationSourceID,
				Status:   debridTypes.TorrentStatusDownloaded,
				Files: map[string]*storage.ProviderFile{
					"movie.mkv": {
						Id:   "source-file",
						Path: "movie.mkv",
						Link: "https://source.invalid/movie.mkv",
					},
				},
			},
		},
		Files: map[string]*storage.File{
			"movie.mkv": {
				Name:     "movie.mkv",
				Path:     "movie.mkv",
				Size:     1024,
				InfoHash: infoHash,
			},
		},
	}
	if err := store.AddOrUpdateDurable(entry); err != nil {
		t.Fatal(err)
	}
	harness.entry = entry
	return harness
}

func (h *migrationOwnershipHarness) execute(keepOld bool) *storage.SwitcherJob {
	job := &storage.SwitcherJob{
		ID:             "migration-test",
		InfoHash:       h.entry.InfoHash,
		SourceProvider: migrationSourceProvider,
		TargetProvider: migrationTargetProvider,
		Status:         storage.SwitcherStatusPending,
		CreatedAt:      time.Now(),
		KeepOld:        keepOld,
	}
	h.manager.executeMigration(job, h.entry)
	return job
}

func assertNoProviderDelete(t *testing.T, deletes <-chan string, provider string) {
	t.Helper()
	select {
	case id := <-deletes:
		t.Fatalf("unexpected delete on %s for torrent ID %q", provider, id)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestExecuteMigrationDeletesSourceOnlyAcrossSameProviderAccounts(t *testing.T) {
	harness := newMigrationOwnershipHarness(t)

	job := harness.execute(false)
	if job.Status != storage.SwitcherStatusCompleted || job.Error != "" {
		t.Fatalf("migration job = status %q, error %q", job.Status, job.Error)
	}
	select {
	case id := <-harness.sourceDeletes:
		if id != migrationSourceID {
			t.Fatalf("source deletion ID = %q, want %q", id, migrationSourceID)
		}
	default:
		t.Fatal("source placement was not deleted")
	}
	assertNoProviderDelete(t, harness.targetDeletes, migrationTargetProvider)

	durable, err := harness.manager.GetEntry(harness.entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if durable.ActiveProvider != migrationTargetProvider {
		t.Fatalf("active provider = %q, want %q", durable.ActiveProvider, migrationTargetProvider)
	}
	if _, exists := durable.Providers[migrationSourceProvider]; exists {
		t.Fatal("durable source placement remains after successful cleanup")
	}
	if target := durable.Providers[migrationTargetProvider]; target == nil || target.ID != migrationTargetID {
		t.Fatalf("durable target placement = %#v", target)
	}
}

func TestExecuteMigrationKeepOldRetainsBothPlacementsWithoutDeletion(t *testing.T) {
	harness := newMigrationOwnershipHarness(t)

	job := harness.execute(true)
	if job.Status != storage.SwitcherStatusCompleted || job.Error != "" {
		t.Fatalf("migration job = status %q, error %q", job.Status, job.Error)
	}
	assertNoProviderDelete(t, harness.sourceDeletes, migrationSourceProvider)
	assertNoProviderDelete(t, harness.targetDeletes, migrationTargetProvider)

	durable, err := harness.manager.GetEntry(harness.entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if durable.ActiveProvider != migrationTargetProvider {
		t.Fatalf("active provider = %q, want %q", durable.ActiveProvider, migrationTargetProvider)
	}
	if durable.Providers[migrationSourceProvider] == nil || durable.Providers[migrationTargetProvider] == nil {
		t.Fatalf("keep-old placements = %#v", durable.Providers)
	}
}

func TestExecuteMigrationCleanupFailureRetainsSourceForRetry(t *testing.T) {
	harness := newMigrationOwnershipHarness(t)
	cleanupErr := errors.New("source provider unavailable")
	harness.sourceErr = cleanupErr

	job := harness.execute(false)
	if job.Status != storage.SwitcherStatusFailed {
		t.Fatalf("migration status = %q, want failed", job.Status)
	}
	if !strings.Contains(job.Error, cleanupErr.Error()) {
		t.Fatalf("migration error = %q, want %q", job.Error, cleanupErr)
	}
	select {
	case id := <-harness.sourceDeletes:
		if id != migrationSourceID {
			t.Fatalf("failed source cleanup ID = %q, want %q", id, migrationSourceID)
		}
	default:
		t.Fatal("source cleanup was not attempted")
	}
	assertNoProviderDelete(t, harness.targetDeletes, migrationTargetProvider)

	durable, err := harness.manager.GetEntry(harness.entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if durable.ActiveProvider != migrationTargetProvider ||
		durable.Providers[migrationSourceProvider] == nil ||
		durable.Providers[migrationTargetProvider] == nil {
		t.Fatalf("recoverable placements after cleanup failure = %#v, active %q", durable.Providers, durable.ActiveProvider)
	}

	// A retry should reuse the already-durable target instead of submitting a
	// duplicate, then remove only the source placement once cleanup recovers.
	harness.sourceErr = nil
	harness.entry = durable
	retry := harness.execute(false)
	if retry.Status != storage.SwitcherStatusCompleted || retry.Error != "" {
		t.Fatalf("retry job = status %q, error %q", retry.Status, retry.Error)
	}
	if calls := harness.submitCalls.Load(); calls != 1 {
		t.Fatalf("target submissions = %d, want 1 across the initial move and retry", calls)
	}
	select {
	case id := <-harness.sourceDeletes:
		if id != migrationSourceID {
			t.Fatalf("retried source cleanup ID = %q, want %q", id, migrationSourceID)
		}
	default:
		t.Fatal("source cleanup was not retried")
	}
	assertNoProviderDelete(t, harness.targetDeletes, migrationTargetProvider)

	durable, err = harness.manager.GetEntry(harness.entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := durable.Providers[migrationSourceProvider]; exists {
		t.Fatal("source placement remains after successful cleanup retry")
	}
}

func TestExecuteMigrationNeverDeletesSourceBeforeTargetIsDurable(t *testing.T) {
	harness := newMigrationOwnershipHarness(t)

	// Advance the durable generation behind the migration's snapshot. This
	// makes the target-placement checkpoint fail like a concurrent update would.
	current, err := harness.manager.GetEntry(harness.entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	current.Category = "generation-advanced"
	if err := harness.manager.AddOrUpdate(current, nil); err != nil {
		t.Fatal(err)
	}

	job := harness.execute(false)
	if job.Status != storage.SwitcherStatusFailed {
		t.Fatalf("migration status = %q, want failed", job.Status)
	}
	if !strings.Contains(job.Error, "persist target placement") {
		t.Fatalf("migration error = %q, want target durability failure", job.Error)
	}
	assertNoProviderDelete(t, harness.sourceDeletes, migrationSourceProvider)
	assertNoProviderDelete(t, harness.targetDeletes, migrationTargetProvider)

	durable, err := harness.manager.GetEntry(harness.entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if durable.ActiveProvider != migrationSourceProvider || durable.Providers[migrationTargetProvider] != nil {
		t.Fatalf("durable state crossed a failed target checkpoint: active %q, placements %#v", durable.ActiveProvider, durable.Providers)
	}
}

func TestExecuteMigrationRollsBackOnlyFailedTargetSubmission(t *testing.T) {
	harness := newMigrationOwnershipHarness(t)
	harness.targetCheckErr = errors.New("target status unavailable")

	job := harness.execute(false)
	if job.Status != storage.SwitcherStatusFailed || !strings.Contains(job.Error, harness.targetCheckErr.Error()) {
		t.Fatalf("migration job = status %q, error %q", job.Status, job.Error)
	}
	select {
	case id := <-harness.targetDeletes:
		if id != migrationTargetID {
			t.Fatalf("target rollback ID = %q, want %q", id, migrationTargetID)
		}
	default:
		t.Fatal("failed target submission was not rolled back synchronously")
	}
	assertNoProviderDelete(t, harness.sourceDeletes, migrationSourceProvider)

	durable, err := harness.manager.GetEntry(harness.entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if durable.ActiveProvider != migrationSourceProvider || durable.Providers[migrationTargetProvider] != nil {
		t.Fatalf("failed target changed durable state: active %q, placements %#v", durable.ActiveProvider, durable.Providers)
	}
}

func TestExecuteMigrationRollsBackTargetReturnedWithSubmitError(t *testing.T) {
	harness := newMigrationOwnershipHarness(t)
	harness.targetSubmitErr = errors.New("target submit response failed")

	job := harness.execute(false)
	if job.Status != storage.SwitcherStatusFailed || !strings.Contains(job.Error, harness.targetSubmitErr.Error()) {
		t.Fatalf("migration job = status %q, error %q", job.Status, job.Error)
	}
	select {
	case id := <-harness.targetDeletes:
		if id != migrationTargetID {
			t.Fatalf("target rollback ID = %q, want %q", id, migrationTargetID)
		}
	default:
		t.Fatal("target returned with a submit error was not rolled back synchronously")
	}
	assertNoProviderDelete(t, harness.sourceDeletes, migrationSourceProvider)

	durable, err := harness.manager.GetEntry(harness.entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if durable.ActiveProvider != migrationSourceProvider || durable.Providers[migrationTargetProvider] != nil {
		t.Fatalf("failed submit changed durable state: active %q, placements %#v", durable.ActiveProvider, durable.Providers)
	}
}
