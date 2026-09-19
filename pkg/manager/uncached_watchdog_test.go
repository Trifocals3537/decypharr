package manager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestParseUncachedStallTimeoutFailsClosed(t *testing.T) {
	for _, test := range []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{raw: "", want: 0},
		{raw: "2h", want: 2 * time.Hour},
		{raw: "9m", wantErr: true},
		{raw: "invalid", wantErr: true},
	} {
		got, err := parseUncachedStallTimeout(test.raw)
		if (err != nil) != test.wantErr || got != test.want {
			t.Fatalf("parseUncachedStallTimeout(%q) = %s, %v; want %s, error=%t", test.raw, got, err, test.want, test.wantErr)
		}
	}
}

func TestUncachedTransferStalledRequiresDefiniteInactivity(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	progressAt := now.Add(-2 * time.Hour)
	observedAt := now
	base := storage.Entry{
		Protocol:         config.ProtocolTorrent,
		DownloadUncached: true,
		State:            storage.EntryStateDownloading,
		Status:           debridTypes.TorrentStatusDownloading,
		Progress:         0.25,
		LastProgressAt:   &progressAt,
		LastObservedAt:   &observedAt,
	}
	if !uncachedTransferStalled(&base, debridTypes.TorrentStatusDownloading, now, time.Hour) {
		t.Fatal("definitely inactive uncached transfer was not classified as stalled")
	}

	checks := []struct {
		name           string
		providerStatus debridTypes.TorrentStatus
		mutate         func(*storage.Entry)
	}{
		{name: "cached policy", providerStatus: debridTypes.TorrentStatusDownloading, mutate: func(e *storage.Entry) { e.DownloadUncached = false }},
		{name: "provider speed", providerStatus: debridTypes.TorrentStatusDownloading, mutate: func(e *storage.Entry) { e.Speed = 1 }},
		{name: "recent progress", providerStatus: debridTypes.TorrentStatusDownloading, mutate: func(e *storage.Entry) { recent := now.Add(-time.Minute); e.LastProgressAt = &recent }},
		{name: "no baseline", providerStatus: debridTypes.TorrentStatusDownloading, mutate: func(e *storage.Entry) { e.LastProgressAt = nil }},
		{name: "generic error", providerStatus: debridTypes.TorrentStatusDownloading, mutate: func(e *storage.Entry) { e.State = storage.EntryStateError }},
		{name: "completed progress", providerStatus: debridTypes.TorrentStatusDownloading, mutate: func(e *storage.Entry) { e.Progress = 1 }},
		{name: "provider completed", providerStatus: debridTypes.TorrentStatusDownloaded, mutate: func(*storage.Entry) {}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			candidate := base
			check.mutate(&candidate)
			if uncachedTransferStalled(&candidate, check.providerStatus, now, time.Hour) {
				t.Fatal("uncertain or active transfer was classified as stalled")
			}
		})
	}
}

func TestQueuedProviderPollPersistsStalledHandoffState(t *testing.T) {
	m := newQueueRecoveryTestManager(t)
	m.uncachedStallTimeout = time.Hour
	entry := interruptedQueueTestEntry("stalled-provider-transfer")
	entry.Status = debridTypes.TorrentStatusDownloading
	entry.IsDownloading = false
	entry.Progress = 0.25
	stale := time.Now().Add(-2 * time.Hour)
	entry.LastProgressAt = &stale
	if err := m.queue.Add(entry); err != nil {
		t.Fatal(err)
	}
	m.clients.Store("torbox", &routingTestClient{
		cfg: config.Debrid{Name: "torbox"},
		check: func(torrent *debridTypes.Torrent) (*debridTypes.Torrent, error) {
			torrent.Debrid = "torbox"
			torrent.Name = "release.mkv"
			torrent.Status = debridTypes.TorrentStatusDownloading
			torrent.Progress = 25
			torrent.Speed = 0
			return torrent, nil
		},
	})

	if err := m.processQueuedTorrent(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	current, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != storage.EntryStateStalledDL || current.Status != debridTypes.TorrentStatusError {
		t.Fatalf("stalled state = %s/%s, want stalledDL/error", current.State, current.Status)
	}
	if current.LastError == "" || current.LastProgressAt == nil || current.LastProgressAt.Unix() != stale.Unix() {
		t.Fatalf("stalled evidence was not preserved: error=%q progressAt=%v", current.LastError, current.LastProgressAt)
	}
}

func TestHandoffStalledUncachedTargetsOwningArr(t *testing.T) {
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/queue":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"page":1,"pageSize":200,"totalRecords":1,"records":[{"id":17,"downloadId":"ABC123","protocol":"torrent"}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v3/queue/bulk":
			deletes.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	store := newLifecycleTestStorage(t)
	queue := newLifecycleTestQueue(store, newEntryLifecycle())
	entry := &storage.Entry{
		InfoHash:         "abc123",
		Name:             "release",
		Protocol:         config.ProtocolTorrent,
		Category:         "sonarr",
		State:            storage.EntryStateStalledDL,
		DownloadUncached: true,
	}
	if err := queue.Add(entry); err != nil {
		t.Fatal(err)
	}
	arrs := arr.NewStorage()
	arrs.AddOrUpdate(arr.New("sonarr", server.URL, "token", false, nil, "", "manual"))
	m := &Manager{queue: queue, arr: arrs, logger: zerolog.Nop()}
	if err := m.handoffStalledUncached(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deletes.Load() != 1 {
		t.Fatalf("Arr delete requests = %d, want 1", deletes.Load())
	}
}

func TestLegacyStalledCleanupPreservesWatchdogHandoff(t *testing.T) {
	store := newLifecycleTestStorage(t)
	queue := newLifecycleTestQueue(store, newEntryLifecycle())
	queue.removeStalledAfter = 10 * time.Minute
	old := time.Now().Add(-time.Hour)

	watchdog, _ := addLifecycleTestEntry(t, queue, "watchdog-stall")
	watchdog.State = storage.EntryStateStalledDL
	watchdog.Status = debridTypes.TorrentStatusError
	watchdog.DownloadUncached = true
	watchdog.AddedOn = old
	if err := queue.Update(watchdog); err != nil {
		t.Fatal(err)
	}

	if err := queue.DeleteStalled(); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.GetTorrent(watchdog.InfoHash); err != nil {
		t.Fatalf("watchdog handoff row was removed before Arr acknowledgement: %v", err)
	}
}

func TestLegacyStalledCleanupStillRemovesOrdinaryErrors(t *testing.T) {
	store := newLifecycleTestStorage(t)
	queue := newLifecycleTestQueue(store, newEntryLifecycle())
	queue.removeStalledAfter = 10 * time.Minute

	entry, _ := addLifecycleTestEntry(t, queue, "ordinary-stall")
	entry.State = storage.EntryStateError
	entry.Status = debridTypes.TorrentStatusError
	entry.AddedOn = time.Now().Add(-time.Hour)
	if err := queue.Update(entry); err != nil {
		t.Fatal(err)
	}

	if err := queue.DeleteStalled(); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.GetTorrent(entry.InfoHash); !storage.IsQueuedEntryNotFound(err) {
		t.Fatalf("ordinary stalled row was not cleaned up: %v", err)
	}
}
