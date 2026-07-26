package manager

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/link"
	"github.com/sirrobot01/decypharr/pkg/notifications"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet"
	"github.com/sirrobot01/decypharr/pkg/version"
	"golang.org/x/sync/singleflight"
)

// Manager handles unified torrent management - replaces wire.Store completely
type Manager struct {
	storage      *storage.Storage
	migrator     *Migrator
	repair       *Repair
	clients      *xsync.Map[string, debrid.Client]
	arr          *arr.Storage
	logger       zerolog.Logger
	ready        chan struct{}
	readyOnce    sync.Once
	streamClient *http.Client

	// Migration jobs tracking
	migrationJobs   *xsync.Map[string, *storage.SwitcherJob]
	refreshInterval time.Duration

	config *config.Config

	// Processing workers
	scheduler    gocron.Scheduler
	cetScheduler gocron.Scheduler
	queue        *Queue

	// downloading
	refreshSG   singleflight.Group
	linkService *link.Service

	// repair
	fixer  *Fixer
	ctx    context.Context
	cancel context.CancelFunc

	background            sync.WaitGroup
	backgroundMu          sync.Mutex
	backgroundStopping    bool
	backgroundWaitTimeout time.Duration

	customFolders *CustomFolders
	mountManager  MountManager

	startTime     time.Time
	usenetTimeout time.Duration

	rootInfo   *FileInfo
	entry      *EntryCache
	downloader *Downloader
	usenet     *usenet.Usenet

	// Debrid speed test results storage
	debridSpeedTestResults *xsync.Map[string, debridTypes.SpeedTestResult]

	// Active streams tracking
	activeStreams *xsync.Map[string, *ActiveStream]

	// In-flight queue-processor dispatches, keyed by InfoHash, to prevent
	// duplicate goroutines from processing the same entry when the scheduler
	// re-fires before the previous pass has updated the queue row.
	processingEntries *xsync.Map[string, struct{}]

	// Unified active-download queue for torrent and NZB imports.
	jobQueue  *JobQueue
	nzbSyncMu sync.Mutex

	// Notifications service
	Notifications *notifications.Service
}

// New creates a new Manager instance
func New() *Manager {
	cfg := config.Get()
	_logger := logger.New("manager")

	strg, err := storage.NewStorage(filepath.Join(config.GetMainPath(), "db"))
	if err != nil {
		panic(fmt.Errorf("failed to create manager storage: %w", err))
	}

	// Optimized transport for high-performance streaming with HTTP/2 multiplexing
	// DNS resolver with caching
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,  // Fast connection timeout
		KeepAlive: 30 * time.Second, // Keep connections alive
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tls.NewLRUClientSessionCache(200),
		},
		TLSHandshakeTimeout:    20 * time.Second,
		MaxIdleConns:           1000,
		MaxIdleConnsPerHost:    500,
		MaxConnsPerHost:        500,
		IdleConnTimeout:        120 * time.Second,
		DisableCompression:     false, // Enable compression for better multiplexing
		DialContext:            dialer.DialContext,
		Proxy:                  http.ProxyFromEnvironment,
		MaxResponseHeaderBytes: 1 << 20,  // 1MB header buffer for CDN responses
		WriteBufferSize:        32 << 10, // 32KB write buffer
		ReadBufferSize:         32 << 10, // 32KB read buffer
	}

	streamClient := &http.Client{
		Timeout:   0,
		Transport: transport,
	}

	usenetTimeout, err := utils.ParseDuration(cfg.Usenet.ProcessingTimeout)
	if err != nil {
		usenetTimeout = 10 * time.Minute
	}

	instance := &Manager{
		storage:                strg,
		clients:                xsync.NewMap[string, debrid.Client](),
		logger:                 _logger,
		migrationJobs:          xsync.NewMap[string, *storage.SwitcherJob](),
		config:                 cfg,
		arr:                    arr.NewStorage(),
		queue:                  newQueue(strg, cfg.RemoveStalledAfter),
		ready:                  make(chan struct{}),
		streamClient:           streamClient,
		usenetTimeout:          usenetTimeout,
		debridSpeedTestResults: xsync.NewMap[string, debridTypes.SpeedTestResult](),
		activeStreams:          xsync.NewMap[string, *ActiveStream](),
		processingEntries:      xsync.NewMap[string, struct{}](),
	}

	instance.resetLifecycle()
	instance.init()

	// Create migrator
	return instance
}

func (m *Manager) resetLifecycle() {
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.backgroundMu.Lock()
	m.backgroundStopping = false
	m.backgroundMu.Unlock()
}

// startBackground registers manager-owned work before starting it. Stop closes
// this registration gate before waiting, which makes Add and Wait ordering safe
// and prevents late scheduler callbacks from escaping the shutdown barrier.
func (m *Manager) startBackground(name string, work func()) bool {
	m.backgroundMu.Lock()
	if m.backgroundStopping {
		m.backgroundMu.Unlock()
		return false
	}
	m.background.Add(1)
	m.backgroundMu.Unlock()

	go func() {
		defer m.background.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				m.logger.Error().
					Str("task", name).
					Interface("panic", recovered).
					Bytes("stack", debug.Stack()).
					Msg("Recovered from panic in background manager task")
			}
		}()
		work()
	}()
	return true
}

func (m *Manager) stopAcceptingBackgroundWork() {
	m.backgroundMu.Lock()
	m.backgroundStopping = true
	m.backgroundMu.Unlock()
}

const defaultBackgroundWaitTimeout = 30 * time.Second

func (m *Manager) waitForBackground() error {
	timeout := m.backgroundWaitTimeout
	if timeout <= 0 {
		timeout = defaultBackgroundWaitTimeout
	}

	done := make(chan struct{})
	go func() {
		m.background.Wait()
		close(done)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return fmt.Errorf("manager background work did not stop within %s", timeout)
	}
}

func (m *Manager) init() {
	cfg := config.Get()
	scheduler, err := gocron.NewScheduler(gocron.WithLocation(time.Local), gocron.WithGlobalJobOptions(gocron.WithTags("decypharr-manager")))
	if err != nil {
		scheduler, _ = gocron.NewScheduler(gocron.WithGlobalJobOptions(gocron.WithTags("decypharr-manager")))
	}

	// Create CET scheduler for time-specific jobs
	cetLocation, err := time.LoadLocation("CET")
	if err != nil {
		cetLocation = time.UTC
	}
	cetScheduler, err := gocron.NewScheduler(gocron.WithLocation(cetLocation), gocron.WithGlobalJobOptions(gocron.WithTags("decypharr-cet")))
	if err != nil {
		cetScheduler, _ = gocron.NewScheduler(gocron.WithGlobalJobOptions(gocron.WithTags("decypharr-cet")))
	}

	m.config = cfg

	// Recreate queue with new config
	m.queue = newQueue(m.storage, cfg.RemoveStalledAfter)

	// Clear debrid clients so they get recreated with new config
	m.clients = xsync.NewMap[string, debrid.Client]()

	// Reset ready channel and syncTorrents.Once for the next start
	m.ready = make(chan struct{})
	m.readyOnce = sync.Once{}

	m.scheduler = scheduler
	m.cetScheduler = cetScheduler
	m.migrator = NewMigrator(m.storage)
	m.downloader = NewDownloadManager(m)

	// Initialize HTTP pool for streaming
	// Note: We can't create a single pool for all files because the LinkRefresh callback
	// needs torrent+filename context. Instead, manager.Stream will create a pool per request
	// and cache it. This is actually better because different files may have different
	// download links from different CDNs.

	refreshInterval, err := utils.ParseDuration(cfg.RefreshInterval)
	if err != nil {
		refreshInterval = 15 * time.Minute
	}
	m.refreshInterval = refreshInterval

	// initialize debrid clients
	m.initDebridClients()

	// Initialize usenet client
	m.initUsenet()

	// Initialize link service
	m.initLinkService()

	// Init custom folders
	m.initCustomFolders()

	// Initialize fixer
	m.fixer = NewFixer(m)

	// Set mount paths
	m.setMountPaths()

	m.initEntryCache()

	// Initialize notifications service
	m.Notifications = notifications.New(&m.config.Notifications, m.logger)

	// Initialize repair service. It registers with the scheduler in StartWorker.
	m.repair = NewRepair(m)

	// Initialize the unified active-download queue after all processors exist.
	m.initJobQueue()
}

func (m *Manager) initUsenet() {
	usenetClient, err := usenet.New()
	if err != nil {
		m.logger.Warn().Msg("Usenet client not configured")
		m.usenet = nil
		return
	}
	m.usenet = usenetClient
}

// initLinkService initializes the link service
func (m *Manager) initLinkService() {
	m.linkService = link.New(
		m.clients,
		m.refreshTorrent,
		m.ReinsertEntry,
		func(entry *storage.Entry) error { return m.AddOrUpdate(entry, nil) },
		m.streamClient,
		m.config.Retries,
		logger.New("link"),
	)
}

func (m *Manager) initJobQueue() {
	m.jobQueue = NewJobQueue(m.ctx, m.config.MaxActiveDownloads, m.processJob)
	// Restore persisted active/queued downloads in the background. With large
	// queues this re-parses thousands of NZBs over the network, and running it
	// inline blocked manager construction — and therefore the HTTP server —
	// for 60-90 minutes on big libraries, during which every arr reported
	// "download client unavailable". Backgrounding lets the API serve and the
	// worker pool drain immediately while the restore catches up.
	m.startBackground("restore active downloads", func() {
		m.restoreActiveDownloadJobs(m.ctx)
	})
}

func (m *Manager) processJob(ctx context.Context, job *Job) {
	if job == nil {
		return
	}
	if job.Entry != nil && job.Request == nil && job.DebridTorrent == nil && job.NZBMeta == nil && !job.ResumeExisting {
		m.waitForDownloadCompletion(ctx, job.Entry)
		return
	}

	var err error
	switch job.Type {
	case JobTypeTorrent:
		err = m.processTorrentJob(ctx, job)
	case JobTypeNZB:
		err = m.processNZBJob(ctx, job)
	default:
		err = fmt.Errorf("unknown job type: %s", job.Type)
	}

	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if isTooManyActiveDownloads(err) {
			if job.Entry != nil {
				job.Entry.Status = debridTypes.TorrentStatusQueued
				_ = m.queue.Update(job.Entry)
			}
			m.jobQueue.Retry(job, 30*time.Second)
			return
		}
		m.logger.Error().Err(err).Str("job_id", job.ID).Str("type", string(job.Type)).Msg("Active download failed")
		if job.Entry != nil {
			job.Entry.MarkAsError(err)
			_ = m.queue.Update(job.Entry)
		}
		return
	}

	m.waitForDownloadCompletion(ctx, job.Entry)
}

func (m *Manager) waitForDownloadCompletion(ctx context.Context, entry *storage.Entry) {
	if entry == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		current, err := m.queue.GetTorrent(entry.InfoHash)
		if err != nil || current.State != storage.EntryStateDownloading {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) migrate(ctx context.Context) {
	// Check if migration has already been done
	status, err := m.migrator.GetStatus()
	if err == nil && !status.Running && status.Completed > 0 {
		m.logger.Info().
			Int("completed", status.Completed).
			Int("errors", status.Errors).
			Msg("Migration already completed previously")
		return
	}

	// GetReader migration stats to see if there are cache files
	stats, err := m.migrator.GetStats()
	if err != nil {
		m.logger.Warn().Err(err).Msg("Failed to get migration stats")
		return
	}

	cacheFiles, ok := stats["cache_files"].(int)
	if !ok || cacheFiles == 0 {
		return
	}

	cacheTorrents, ok := stats["cache_torrents"].(int)
	if !ok {
		cacheTorrents = 0
	}

	m.logger.Info().
		Int("cache_files", cacheFiles).
		Int("unique_torrents", cacheTorrents).
		Msg("Found cache files, starting automatic migration...")

	// Start migration with backup
	if err := m.migrator.Start(ctx); err != nil {
		m.logger.Error().Err(err).Msg("Failed to start automatic migration")
		return
	}

	m.logger.Info().Msg("Automatic migration started successfully")
}

// Start starts the manager and all its components
func (m *Manager) Start(ctx context.Context) error {
	m.startTime = time.Now()
	m.logger.Info().
		Str("version", version.GetInfo().String()).
		Str("mount_type", string(m.config.Mount.Type)).
		Str("notifications", fmt.Sprintf("%v", m.Notifications.IsEnabled())).
		Str("mount_path", m.config.Mount.MountPath).
		Msg("Starting manager")

	// Tie manager-owned startup work to the service lifetime. Stop also cancels
	// this context, so restarts and direct shutdowns use the same path.
	m.startBackground("service cancellation", func() {
		select {
		case <-ctx.Done():
			if m.cancel != nil {
				m.cancel()
			}
		case <-m.ctx.Done():
		}
	})

	// run the migration process
	m.migrate(m.ctx)

	m.startBackground("initial provider sync", func() {
		m.syncTorrents(m.ctx)
		if m.ctx.Err() != nil {
			return
		}
		// Sync NZBs
		if err := m.syncNZBs(m.ctx); err != nil && m.ctx.Err() == nil {
			m.logger.Error().Err(err).Msg("Failed to perform initial NZB syncTorrents")
		}
		if fixNZB := os.Getenv("DECYPHARR_FIX_NZB_SIZES"); fixNZB == "1" {
			m.logger.Info().Msg("Starting NZB file size correction as requested by environment variable")
			m.fixNZBFileSizes(m.ctx)
		}
	})

	// Start workers
	if err := m.StartWorker(m.ctx); err != nil {
		return fmt.Errorf("failed to start manager worker: %w", err)
	}

	// Close ready channel once, safe for multiple calls
	m.readyOnce.Do(func() {
		close(m.ready)
	})

	// Start the mount manager if set
	// This also start thr mounting process
	if m.mountManager != nil {
		if err := m.mountManager.Start(ctx); err != nil {
			// If mount manager fails to start, we log the error but continue running the manager
			m.logger.Error().Err(err).Msg("Failed to start mount manager, continuing without mounting")
			return nil
		}
	}

	return nil
}

// Stop stops the manager and cleans up all resources
func (m *Manager) Stop() error {
	m.logger.Info().Msg("Stopping manager")
	var shutdownErr error

	// Close the background registration gate before cancellation. Any work
	// accepted before this point is included in the barrier; later scheduler
	// callbacks are rejected.
	m.stopAcceptingBackgroundWork()

	if m.cancel != nil {
		m.cancel()
	}

	if m.migrator != nil {
		if err := m.migrator.Stop(); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("failed to stop migration: %w", err))
		}
	}

	// Stop schedulers
	if m.scheduler != nil {
		if err := m.scheduler.Shutdown(); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("failed to shutdown scheduler: %w", err))
		} else {
			// Shutdown consumes the scheduler's terminal result. Clear the
			// handle so a retry after a later drain timeout remains safe.
			m.scheduler = nil
		}
	}
	if m.cetScheduler != nil {
		if err := m.cetScheduler.Shutdown(); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("failed to shutdown CET scheduler: %w", err))
		} else {
			m.cetScheduler = nil
		}
	}

	if m.jobQueue != nil {
		m.logger.Info().Msg("Closing active download queue")
		m.jobQueue.Close()
	}

	if m.repair != nil {
		if err := m.repair.Stop(); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("failed to stop repair service: %w", err))
		}
	}

	// All manager-owned startup work must finish before mount, usenet, or
	// storage resources are closed. A provider API may not expose a
	// context-aware method; in that case return a bounded error and preserve
	// those resources so the unfinished task can never run against closed
	// state.
	if err := m.waitForBackground(); err != nil {
		shutdownErr = errors.Join(shutdownErr, err)
	}
	if shutdownErr != nil {
		m.logger.Error().Err(shutdownErr).Msg("Manager shutdown paused with resources still open")
		return shutdownErr
	}

	if m.mountManager != nil {
		m.logger.Info().Msg("Stopping mount manager")
		if err := m.mountManager.Stop(); err != nil {
			return fmt.Errorf("failed to stop mount manager: %w", err)
		}
	}

	// Close usenet connection manager if active
	if m.usenet != nil {
		m.logger.Info().Msg("Closing usenet connections")
		if err := m.usenet.Close(); err != nil {
			return fmt.Errorf("failed to close usenet: %w", err)
		}
	}

	// Close storage
	if m.storage != nil {
		m.logger.Info().Msg("Closing storage database")
		if err := m.storage.Close(); err != nil {
			return fmt.Errorf("failed to close storage: %w", err)
		}
	}

	m.logger.Info().Msg("Manager stopped successfully")
	return nil
}

// Reset resets the manager with the new configuration
// This is called after config changes (e.g., setup wizard) to apply new settings
func (m *Manager) Reset() error {
	m.logger.Info().Msg("Resetting manager with new configuration")

	// Stop resources before resetting
	if err := m.Stop(); err != nil {
		return fmt.Errorf("failed to stop manager during reset: %w", err)
	}

	// Reopen storage database (it was closed by Stop)
	strg, err := storage.NewStorage(filepath.Join(config.GetMainPath(), "db"))
	if err != nil {
		return fmt.Errorf("failed to reopen storage after reset: %w", err)
	}
	m.storage = strg

	m.resetLifecycle()

	// Reload configuration
	m.init()
	m.logger.Info().Msg("Manager reset complete")
	return nil
}

func (m *Manager) GetStats() (map[string]any, error) {
	count, err := m.storage.Count()
	if err != nil {
		return nil, err
	}

	diskSize := m.storage.DiskSize()
	activeJobs := 0
	completedJobs := 0
	failedJobs := 0
	m.migrationJobs.Range(func(_ string, job *storage.SwitcherJob) bool {
		switch job.Status {
		case storage.SwitcherStatusPending, storage.SwitcherStatusInProgress:
			activeJobs++
		case storage.SwitcherStatusCompleted:
			completedJobs++
		case storage.SwitcherStatusFailed, storage.SwitcherStatusCancelled:
			failedJobs++
		}
		return true
	})

	return map[string]any{
		"total_torrents": count,
		"storage_stats":  map[string]any{"total_size": diskSize},
		"active_jobs":    activeJobs,
		"completed_jobs": completedJobs,
		"failed_jobs":    failedJobs,
	}, nil
}

func (m *Manager) IsReady() chan struct{} {
	return m.ready
}

func (m *Manager) Uptime() time.Duration {
	return time.Since(m.startTime)
}

func (m *Manager) StartTime() time.Time {
	return m.startTime
}

// CRUD operations

func (m *Manager) GetEntryItem(torrentName string) (*storage.EntryItem, error) {
	return m.storage.GetEntryItem(torrentName)
}

func (m *Manager) GetEntryByName(torrentName, filename string) (*storage.Entry, error) {
	// First get entry
	entry, err := m.storage.GetEntryItem(torrentName)
	if err != nil {
		return nil, err
	}

	// Find the file in the entry
	file, err := entry.GetFile(filename)
	if err != nil {
		return nil, err
	}
	return m.GetEntry(file.InfoHash)
}

func (m *Manager) AddOrUpdate(entry *storage.Entry, callback func(t *storage.Entry)) error {
	entry.UpdatedAt = time.Now()
	if err := m.storage.AddOrUpdate(entry); err != nil {
		return err
	}
	if callback != nil {
		m.startBackground("entry update callback", func() {
			callback(entry)
		})
	}
	return nil
}

// GetEntry gets a torrent by name
func (m *Manager) GetEntry(infohash string) (*storage.Entry, error) {
	return m.storage.Get(infohash)
}

func (m *Manager) EntryExists(infohash string) (bool, error) {
	return m.storage.Exists(infohash)
}

func (m *Manager) GetTorrents(filter func(*storage.Entry) bool) ([]*storage.Entry, error) {
	// Use streaming to avoid loading all torrents into memory at once
	var torrents []*storage.Entry
	err := m.storage.ForEach(func(t *storage.Entry) error {
		if filter == nil || filter(t) {
			torrents = append(torrents, t)
		}
		return nil
	})
	return torrents, err
}

func (m *Manager) GetTorrentsCount() (int, error) {
	return m.storage.Count()
}

// DeleteEntry deletes a torrent by infohash
func (m *Manager) DeleteEntry(infohash string, removePlacements bool) error {
	torr, err := m.GetEntry(infohash)
	if err != nil {
		return err
	}
	// Delete active placements from debrid clients
	if removePlacements {
		if !m.startBackground("remove torrent placements", func() {
			m.RemoveTorrentPlacements(torr)
		}) {
			return fmt.Errorf("cannot delete entry while manager is stopping")
		}
	}

	if err := m.storage.Delete(infohash); err != nil {
		return err
	}
	// Refresh entry cache
	m.RefreshEntries(true)
	return nil
}

func (m *Manager) DeleteTorrents(infohashes []string, removeFromDebrid bool) error {
	for _, infohash := range infohashes {
		if err := m.DeleteEntry(infohash, removeFromDebrid); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) GetMigrationJob(jobID string) (*storage.SwitcherJob, error) {
	job, exists := m.migrationJobs.Load(jobID)
	if !exists {
		return nil, fmt.Errorf("migration job not found: %s", jobID)
	}
	return job, nil
}

// SubmitJob submits an import to the unified active-download queue.
func (m *Manager) SubmitJob(job *Job) error {
	if m.jobQueue == nil {
		return fmt.Errorf("active download queue not initialized")
	}
	return m.jobQueue.Submit(job)
}
