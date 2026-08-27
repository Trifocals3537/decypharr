package manager

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/testutil"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestAdmitNewTorrentReturnsBeforeProviderAndSuppressesDuplicates(t *testing.T) {
	providerStarted := make(chan struct{})
	providerRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseProvider := func() {
		releaseOnce.Do(func() { close(providerRelease) })
	}
	var attempts atomic.Int32
	provider := &routingTestClient{
		cfg: config.Debrid{Name: "torbox"},
		submit: func(*debridTypes.Torrent) (*debridTypes.Torrent, error) {
			if attempts.Add(1) == 1 {
				close(providerStarted)
			}
			<-providerRelease
			return nil, errors.New("provider temporarily unavailable")
		},
	}
	manager, downloadRoot := newAsyncAdmissionTestManager(t, provider, 1)
	// Register after the manager so this runs first (test cleanups are LIFO),
	// preventing a failed assertion from leaving Close waiting on the fake.
	t.Cleanup(releaseProvider)
	downloadUncached := true
	request := asyncAdmissionTestRequest(downloadRoot, "first", &downloadUncached)

	if err := admitTorrentPromptly(t, manager, request); err != nil {
		t.Fatalf("AdmitNewTorrent() error: %v", err)
	}
	select {
	case <-providerStarted:
	case <-time.After(time.Second):
		t.Fatal("provider worker did not start")
	}

	entry, err := manager.queue.GetTorrent(request.Magnet.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Status != debridTypes.TorrentStatusQueued || entry.State != storage.EntryStateDownloading {
		t.Fatalf("admitted entry state = %s/%s, want queued/downloading", entry.Status, entry.State)
	}
	if entry.ActiveProvider != "torbox" || !entry.DownloadUncached {
		t.Fatalf("persisted admission policy = provider %q uncached %t", entry.ActiveProvider, entry.DownloadUncached)
	}
	rebuilt, err := manager.rebuildQueuedTorrentJob(entry)
	if err != nil {
		t.Fatalf("rebuild queued job: %v", err)
	}
	if rebuilt.Request == nil || rebuilt.Request.SelectedDebrid != "torbox" ||
		rebuilt.Request.DownloadUncached == nil || !*rebuilt.Request.DownloadUncached {
		t.Fatalf("rebuilt admission policy = %#v", rebuilt.Request)
	}

	duplicate := asyncAdmissionTestRequest(downloadRoot, "first", &downloadUncached)
	if err := admitTorrentPromptly(t, manager, duplicate); !errors.Is(err, ErrJobQueueDuplicate) {
		t.Fatalf("duplicate admission error = %v, want %v", err, ErrJobQueueDuplicate)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("provider attempts before release = %d, want 1", got)
	}

	releaseProvider()
	waitForQueuedEntryState(t, manager.queue, request.Magnet.InfoHash, storage.EntryStateError)
	entry, err = manager.queue.GetTorrent(request.Magnet.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(entry.LastError, "provider temporarily unavailable") {
		t.Fatalf("queue error = %q, want provider failure", entry.LastError)
	}
}

func TestAdmitNewTorrentPersistsAndRebuildsExactTorrentSource(t *testing.T) {
	torrentData, err := os.ReadFile(testutil.GetTestTorrentPath())
	if err != nil {
		t.Fatal(err)
	}
	magnet, err := utils.GetMagnetFromBytes(torrentData, false)
	if err != nil {
		t.Fatal(err)
	}
	submitted := make(chan []byte, 1)
	provider := &routingTestClient{
		cfg: config.Debrid{Name: "torbox"},
		submit: func(torrent *debridTypes.Torrent) (*debridTypes.Torrent, error) {
			submitted <- append([]byte(nil), torrent.Magnet.File...)
			return nil, errors.New("stop after source capture")
		},
	}
	manager, downloadRoot := newAsyncAdmissionTestManager(t, provider, 1)
	request := NewTorrentRequest(
		"torbox",
		downloadRoot,
		magnet,
		&arr.Arr{Name: "sonarr"},
		config.DownloadActionSymlink,
		nil,
		"",
		ImportTypeQBit,
		false,
	)

	if err := admitTorrentPromptly(t, manager, request); err != nil {
		t.Fatalf("AdmitNewTorrent() error = %v", err)
	}
	persisted, err := manager.storage.LoadTorrentSource(magnet.InfoHash)
	if err != nil {
		t.Fatalf("LoadTorrentSource() error = %v", err)
	}
	if !bytes.Equal(persisted, torrentData) {
		t.Fatal("persisted source differs from admitted torrent")
	}

	entry, err := manager.queue.GetTorrent(magnet.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := manager.rebuildQueuedTorrentJob(entry)
	if err != nil {
		t.Fatalf("rebuildQueuedTorrentJob() error = %v", err)
	}
	if rebuilt.Request == nil || !bytes.Equal(rebuilt.Request.Magnet.File, torrentData) {
		t.Fatal("rebuilt job did not retain the exact torrent source")
	}

	select {
	case got := <-submitted:
		if !bytes.Equal(got, torrentData) {
			t.Fatal("provider submission did not receive the exact torrent source")
		}
	case <-time.After(time.Second):
		t.Fatal("provider submission did not run")
	}
}

func TestAdmitNewTorrentBoundsProviderSubmissionsByWorkerCount(t *testing.T) {
	providerStarted := make(chan string, 2)
	firstRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() {
		releaseOnce.Do(func() { close(firstRelease) })
	}
	var attempts atomic.Int32
	provider := &routingTestClient{
		cfg: config.Debrid{Name: "torbox"},
		submit: func(torrent *debridTypes.Torrent) (*debridTypes.Torrent, error) {
			attempt := attempts.Add(1)
			providerStarted <- torrent.InfoHash
			if attempt == 1 {
				<-firstRelease
			}
			return nil, errors.New("provider unavailable")
		},
	}
	manager, downloadRoot := newAsyncAdmissionTestManager(t, provider, 1)
	t.Cleanup(releaseFirst)
	first := asyncAdmissionTestRequest(downloadRoot, "first", nil)
	second := asyncAdmissionTestRequest(downloadRoot, "second", nil)

	if err := admitTorrentPromptly(t, manager, first); err != nil {
		t.Fatal(err)
	}
	if err := admitTorrentPromptly(t, manager, second); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-providerStarted:
		if got != first.Magnet.InfoHash {
			t.Fatalf("first provider submission = %q, want %q", got, first.Magnet.InfoHash)
		}
	case <-time.After(time.Second):
		t.Fatal("first provider submission did not start")
	}
	select {
	case got := <-providerStarted:
		t.Fatalf("second provider submission %q escaped the one-worker bound", got)
	case <-time.After(50 * time.Millisecond):
	}

	releaseFirst()
	select {
	case got := <-providerStarted:
		if got != second.Magnet.InfoHash {
			t.Fatalf("second provider submission = %q, want %q", got, second.Magnet.InfoHash)
		}
	case <-time.After(time.Second):
		t.Fatal("second provider submission did not start after capacity released")
	}
	waitForQueuedEntryState(t, manager.queue, first.Magnet.InfoHash, storage.EntryStateError)
	waitForQueuedEntryState(t, manager.queue, second.Magnet.InfoHash, storage.EntryStateError)
}

func newAsyncAdmissionTestManager(
	t *testing.T,
	provider debrid.Client,
	workers int,
) (*Manager, string) {
	t.Helper()
	store := newLifecycleTestStorage(t)
	lifecycle := newEntryLifecycle()
	queue := newLifecycleTestQueue(store, lifecycle)
	downloadRoot := t.TempDir()
	config.Get().DownloadFolder = downloadRoot
	clients := xsync.NewMap[string, debrid.Client]()
	clients.Store("torbox", provider)
	manager := &Manager{
		storage:              store,
		queue:                queue,
		entryLifecycle:       lifecycle,
		clients:              clients,
		arr:                  arr.NewStorage(),
		config:               &config.Config{DownloadFolder: downloadRoot, Debrids: []config.Debrid{{Name: "torbox"}}},
		logger:               zerolog.Nop(),
		submissionRejections: newSubmissionRejectionCache(time.Hour, 16),
	}
	manager.resetLifecycle()
	manager.jobQueue = NewJobQueueWithCapacity(manager.ctx, workers, 8, manager.processJob, lifecycle)
	manager.jobQueue.logger = zerolog.Nop()
	queue.removePendingJobs = manager.jobQueue.DeleteJobs
	t.Cleanup(func() {
		manager.jobQueue.Close()
		manager.cancel()
	})
	return manager, downloadRoot
}

func admitTorrentPromptly(
	t *testing.T,
	manager *Manager,
	request *ImportRequest,
) error {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		result <- manager.AdmitNewTorrent(context.Background(), request)
	}()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("durable admission waited for provider work")
		return nil
	}
}

func asyncAdmissionTestRequest(
	downloadRoot string,
	name string,
	downloadUncached *bool,
) *ImportRequest {
	hash := map[string]string{
		"first":  "1111111111111111111111111111111111111111",
		"second": "2222222222222222222222222222222222222222",
	}[name]
	magnet := &utils.Magnet{
		InfoHash: hash,
		Name:     name,
		Link:     "magnet:?xt=urn:btih:" + hash,
	}
	return NewTorrentRequest(
		"torbox",
		downloadRoot,
		magnet,
		&arr.Arr{Name: "sonarr"},
		config.DownloadActionSymlink,
		downloadUncached,
		"",
		ImportTypeQBit,
		false,
	)
}

func waitForQueuedEntryState(
	t *testing.T,
	queue *Queue,
	infoHash string,
	want storage.TorrentState,
) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entry, err := queue.GetTorrent(infoHash)
		if err == nil && entry.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	entry, err := queue.GetTorrent(infoHash)
	t.Fatalf("queue state = %#v, error = %v, want %s", entry, err, want)
}
