package manager

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet"
)

var errDeleteQueueEntryOnJobFinish = errors.New("delete queue entry after job finishes")

// AdmitNewTorrent durably records a torrent before provider work begins, then
// hands submission and status checks to the bounded active-download worker
// pool. qBittorrent callers therefore wait only for validation and durable
// admission, not for provider latency. A process restart can rebuild the job
// from the queued entry if it stops after the record is committed.
func (m *Manager) AdmitNewTorrent(ctx context.Context, importReq *ImportRequest) error {
	if err := m.validateTorrentImportRequest(importReq); err != nil {
		return err
	}

	reservation, err := m.reserveJob(ctx, importReq.Magnet.InfoHash)
	if err != nil {
		return err
	}
	defer reservation.release()

	torrent := newTorrentQueueEntry(importReq, debridTypes.TorrentStatusQueued)
	if err := m.persistTorrentSource(importReq); err != nil {
		return fmt.Errorf("persist torrent source before admission: %w", err)
	}
	if err := m.queue.Add(torrent); err != nil {
		return errors.Join(
			fmt.Errorf("failed to add torrent to queue: %w", err),
			m.pruneTorrentSourcesAfterFailedAdmission(importReq),
		)
	}

	importReq.Status = "queued"
	importReq.Async = true
	importReq.CompletedAt = time.Time{}
	importReq.Error = ""
	job := NewJob(JobTypeTorrent, importReq)
	job.ID = torrent.InfoHash
	job.Entry = torrent
	if err := m.submitReservedJob(reservation, job); err != nil {
		importReq.Status = "error"
		importReq.Error = err.Error()
		torrent.MarkAsError(err)
		_ = m.queue.Update(torrent)
		return fmt.Errorf("failed to queue torrent: %w", err)
	}
	return nil
}

// AddNewTorrent submits a torrent to debrid before entering the active-download queue.
func (m *Manager) AddNewTorrent(ctx context.Context, importReq *ImportRequest) error {
	if importReq == nil || importReq.Magnet == nil {
		return fmt.Errorf("magnet is required")
	}
	if importReq.Arr == nil {
		return fmt.Errorf("arr is required")
	}
	reservation, err := m.reserveJob(ctx, importReq.Magnet.InfoHash)
	if err != nil {
		return err
	}
	defer reservation.release()

	debridTorrent, err := m.SendToDebrid(reservation.Context(), importReq)
	if err != nil {
		if isTooManyActiveDownloads(err) {
			m.logger.Warn().Msgf("Too many active downloads, marking as queued: %s", importReq.Magnet.Name)
			return m.queueTorrentRetry(importReq, reservation)
		}
		return fmt.Errorf("failed to submit torrent to debrid: %w", err)
	}

	torrent := newTorrentQueueEntry(importReq, debridTypes.TorrentStatusQueued)
	torrent.DownloadUncached = debridTorrent.DownloadUncached
	applyDebridTorrentToEntry(torrent, debridTorrent)

	if err := m.persistTorrentSource(importReq); err != nil {
		rollbackErr := m.deleteProviderTorrent(
			m.ProviderClient(debridTorrent.Debrid),
			debridTorrent.Id,
		)
		return errors.Join(
			fmt.Errorf("persist torrent source before queueing: %w", err),
			rollbackErr,
		)
	}
	if err := m.queue.Add(torrent); err != nil {
		return errors.Join(
			fmt.Errorf("failed to add torrent to queue: %w", err),
			m.pruneTorrentSourcesAfterFailedAdmission(importReq),
		)
	}

	job := NewJob(JobTypeTorrent, importReq)
	job.ID = torrent.InfoHash
	job.Entry = torrent
	job.DebridTorrent = debridTorrent
	if err := m.submitReservedJob(reservation, job); err != nil {
		torrent.MarkAsError(err)
		_ = m.queue.Update(torrent)
		return fmt.Errorf("failed to queue torrent: %w", err)
	}
	return nil
}

func (m *Manager) processTorrentJob(ctx context.Context, job *Job) error {
	if job == nil || job.Entry == nil {
		return fmt.Errorf("invalid torrent job")
	}
	if _, err := m.queue.GetTorrent(job.Entry.InfoHash); err != nil {
		return nil
	}
	if job.ResumeExisting {
		job.Entry.Status = debridTypes.TorrentStatusDownloading
		job.Entry.IsDownloading = false
		_ = m.queue.Update(job.Entry)
		m.processingEntries.Store(job.Entry.InfoHash, struct{}{})
		return m.processQueuedTorrent(ctx, job.Entry)
	}
	if job.DebridTorrent == nil {
		if job.Request == nil {
			m.waitForDownloadCompletion(ctx, job.Entry)
			return nil
		}
		debridTorrent, err := m.SendToDebrid(ctx, job.Request)
		if err != nil {
			return fmt.Errorf("failed to submit torrent to debrid: %w", err)
		}
		job.DebridTorrent = debridTorrent
	}

	job.Entry.Status = debridTypes.TorrentStatusDownloading
	job.Entry.DownloadUncached = job.DebridTorrent.DownloadUncached
	if job.Request != nil {
		job.Request.Status = "started"
	}
	return m.processNewTorrent(ctx, job.Entry, job.DebridTorrent)
}

func (m *Manager) queueTorrentRetry(importReq *ImportRequest, reservation *jobReservation) error {
	torrent := newTorrentQueueEntry(importReq, debridTypes.TorrentStatusQueued)
	if err := m.persistTorrentSource(importReq); err != nil {
		return fmt.Errorf("persist torrent source before retry queueing: %w", err)
	}
	if err := m.queue.Add(torrent); err != nil {
		return errors.Join(
			fmt.Errorf("failed to add torrent to queue: %w", err),
			m.pruneTorrentSourcesAfterFailedAdmission(importReq),
		)
	}

	importReq.Status = "queued"
	importReq.CompletedAt = time.Time{}
	importReq.Error = ""
	job := NewJob(JobTypeTorrent, importReq)
	job.ID = torrent.InfoHash
	job.Entry = torrent
	if err := m.submitReservedJob(reservation, job); err != nil {
		torrent.MarkAsError(err)
		_ = m.queue.Update(torrent)
		return fmt.Errorf("failed to queue torrent: %w", err)
	}
	return nil
}

func newTorrentQueueEntry(importReq *ImportRequest, status debridTypes.TorrentStatus) *storage.Entry {
	now := time.Now()
	torrent := &storage.Entry{
		InfoHash:         importReq.Magnet.InfoHash,
		Name:             importReq.Magnet.Name,
		OriginalFilename: importReq.Magnet.Name,
		Protocol:         config.ProtocolTorrent,
		Size:             importReq.Magnet.Size,
		Bytes:            importReq.Magnet.Size,
		Magnet:           importReq.Magnet.Link,
		Category:         importReq.Arr.Name,
		SavePath:         filepath.Join(importReq.DownloadFolder, importReq.Arr.Name),
		Status:           status,
		State:            storage.EntryStateDownloading,
		Progress:         0,
		Action:           importReq.Action,
		CallbackURL:      importReq.CallBackUrl,
		SkipMultiSeason:  importReq.SkipMultiSeason,
		CreatedAt:        now,
		UpdatedAt:        now,
		AddedOn:          now,
		Providers:        make(map[string]*storage.ProviderEntry),
		Files:            make(map[string]*storage.File),
		Tags:             []string{},
	}
	// Persist the admission policy before provider work starts. ActiveProvider
	// doubles as the requested provider while Providers is still empty; once a
	// placement succeeds applyDebridTorrentToEntry replaces it with the actual
	// provider. This preserves explicit qBittorrent routing across a restart.
	torrent.ActiveProvider = importReq.SelectedDebrid
	if importReq.DownloadUncached != nil {
		torrent.DownloadUncached = *importReq.DownloadUncached
	}
	torrent.ContentPath = torrent.DownloadPath()
	return torrent
}

func isTooManyActiveDownloads(err error) bool {
	customErr, ok := errors.AsType[*customerror.Error](err)
	return ok && customErr.Code == "too_many_active_downloads"
}

func (m *Manager) processQueuedEntries(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	queueEntries := m.queue.ListFilter("", config.ProtocolAll, storage.EntryStateDownloading, nil, "", true)
	if len(queueEntries) == 0 {
		return
	}
	for _, entry := range queueEntries {
		if ctx.Err() != nil {
			return
		}
		// Parse only active downloading torrents
		if entry.State != storage.EntryStateDownloading {
			continue
		}
		if entry.Status == debridTypes.TorrentStatusQueued {
			continue
		}
		// Skip entries that are actively being downloading
		if entry.IsDownloading {
			continue
		}
		// Skip if a previous tick's goroutine hasn't finished yet for this hash.
		if _, loaded := m.processingEntries.LoadOrStore(entry.InfoHash, struct{}{}); loaded {
			continue
		}
		if entry.IsTorrent() {
			if entry.ActiveProvider != "" {
				if !m.startEntryBackground(ctx, "queued torrent processing", entry, func(workCtx context.Context) error {
					return m.processQueuedTorrent(workCtx, entry)
				}) {
					m.processingEntries.Delete(entry.InfoHash)
				}
			} else {
				m.processingEntries.Delete(entry.InfoHash)
			}
		} else if entry.IsNZB() {
			if !m.startEntryBackground(ctx, "queued NZB processing", entry, func(workCtx context.Context) error {
				return m.processQueuedNZB(workCtx, entry)
			}) {
				m.processingEntries.Delete(entry.InfoHash)
			}
		} else {
			m.processingEntries.Delete(entry.InfoHash)
		}
	}
}

func (m *Manager) processQueuedNZB(ctx context.Context, entry *storage.Entry) error {
	defer m.processingEntries.Delete(entry.InfoHash)
	if err := ctx.Err(); err != nil {
		return err
	}
	// Check if the nzb is already processed. Only header fields (status, file
	// list) are needed here; processNZB does not touch the segment map.
	metadata, err := m.usenet.GetNZBHeader(entry.InfoHash)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err != nil {
		m.logger.Error().Err(err).Str("name", entry.Name).Msg("Error getting NZB metadata")
		entry.MarkAsError(err)
		_ = m.queue.Update(entry)
		return nil
	}
	if metadata == nil {
		m.logger.Error().Str("name", entry.Name).Msg("NZB metadata not found")
		entry.MarkAsError(fmt.Errorf("nzb metadata not found"))
		_ = m.queue.Update(entry)
		return nil
	}
	switch metadata.Status {
	case usenet.NZBStatusFailed:
		m.logger.Error().Str("name", entry.Name).Msg("NZB processing failed")
		entry.MarkAsError(fmt.Errorf("nzb processing failed"))
		_ = m.queue.Update(entry)
		return nil
	case usenet.NZBStatusParsing, usenet.NZBStatusDownloading:
		// Still processing, skip for now
		return nil
	case usenet.NZBStatusCompleted:
		if err := m.processNZB(ctx, entry, metadata); err != nil {
			if errors.Is(err, errDeleteQueueEntryOnJobFinish) || errors.Is(err, context.Canceled) {
				return err
			}
			m.logger.Error().Err(err).Str("name", entry.Name).Msg("Error processing queued NZB")
			entry.MarkAsError(err)
			_ = m.queue.Update(entry)
			return nil
		}
	default:
		m.logger.Error().Str("name", entry.Name).Msgf("Unknown NZB status: %s", metadata.Status)
		entry.MarkAsError(fmt.Errorf("unknown nzb status: %s", metadata.Status))
		_ = m.queue.Update(entry)
		return nil
	}
	return nil
}

func (m *Manager) processQueuedTorrent(ctx context.Context, entry *storage.Entry) error {
	defer m.processingEntries.Delete(entry.InfoHash)
	if err := ctx.Err(); err != nil {
		return err
	}
	placement := entry.GetActiveProvider()
	if placement == nil {
		m.logger.Error().Str("name", entry.Name).Msg("No active placement found for queued entry")
		entry.MarkAsError(fmt.Errorf("no active placement found"))
		_ = m.queue.Update(entry)
		return nil
	}

	client := m.ProviderClient(entry.ActiveProvider)
	if client == nil {
		m.logger.Error().Str("debrid", entry.ActiveProvider).Msg("Provider client not found")
		entry.MarkAsError(fmt.Errorf("debrid client not found: %s", entry.ActiveProvider))
		_ = m.queue.Update(entry)
		return nil
	}

	magnet, err := utils.GetMagnetInfo(entry.Magnet, m.config.AlwaysRmTrackerUrls)
	if err != nil {
		magnet = utils.ConstructMagnet(entry.InfoHash, entry.Name)
	}

	arr := m.arr.GetOrCreate(entry.Category)

	debridTorrent := &debridTypes.Torrent{
		Id:               placement.ID,
		InfoHash:         entry.InfoHash,
		Magnet:           magnet,
		Name:             magnet.Name,
		Arr:              arr,
		Size:             entry.Size,
		Files:            make(map[string]debridTypes.File),
		DownloadUncached: entry.DownloadUncached,
	}

	dbT, err := checkProviderStatus(ctx, client, debridTorrent)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		if dbT != nil && dbT.Id != "" {
			err = errors.Join(err, m.deleteProviderTorrent(client, dbT.Id))
		}
		m.logger.Error().Err(err).Str("name", entry.Name).Msg("Error checking status")
		entry.MarkAsError(err)
		_ = m.queue.Update(entry)
		return nil
	}

	debridTorrent = dbT

	if debridTorrent == nil {
		m.logger.Error().Str("name", entry.Name).Msg("Provider entry not found")
		entry.MarkAsError(fmt.Errorf("debrid entry not found"))
		_ = m.queue.Update(entry)
		return nil
	}

	if err := validateTorrentRootName(debridTorrent.Name, false); err != nil {
		err = errors.Join(err, m.deleteProviderTorrent(client, debridTorrent.Id))
		m.logger.Error().Err(err).Str("name", entry.Name).Msg("Provider returned unsafe torrent name")
		entry.MarkAsError(err)
		_ = m.queue.Update(entry)
		return nil
	}

	if debridTorrent.Status == debridTypes.TorrentStatusError {
		m.logger.Error().
			Str("debrid", debridTorrent.Debrid).
			Str("name", debridTorrent.Name).
			Str("status", string(debridTorrent.Status)).
			Msg("Entry in error state")
		entry.MarkAsError(fmt.Errorf("entry in error state on debrid: %s", debridTorrent.Debrid))
		_ = m.queue.Update(entry)
		return nil
	}

	// Update entry progress
	entry.Progress = debridTorrent.Progress / 100.0
	entry.Speed = debridTorrent.Speed
	entry.Size = debridTorrent.GetSize()
	entry.Seeders = debridTorrent.Seeders
	entry.UpdatedAt = time.Now()

	// Update placement progress
	if placement := entry.GetActiveProvider(); placement != nil {
		placement.Progress = entry.Progress
	}

	_ = m.queue.Update(entry)
	// Check if done or failed
	if debridTorrent.Status == debridTypes.TorrentStatusDownloaded {
		return m.processAction(ctx, entry)
	}
	return nil
}

func (m *Manager) processAction(ctx context.Context, entry *storage.Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entry.Status = debridTypes.TorrentStatusDownloaded
	entry.UpdatedAt = time.Now()
	if err := m.queue.Update(entry); err != nil {
		return err
	}
	m.logger.Info().
		Str("name", entry.Name).
		Str("action", string(entry.Action)).
		Msg("Download completed, processing action")

	// Merge with existing entry if same infohash already exists (e.g., same
	// torrent on a different provider). The queue entry only knows about the
	// provider it was queued for, so we need to preserve other placements.
	if existing, err := m.storage.Get(entry.InfoHash); err == nil && existing != nil {
		entry = storage.HandleExistingEntryMerge(existing, entry)
	}

	// Explicit same-key reimports are authorized only by the exact durable
	// queue incarnation created after the prior main entry was retired. This
	// transient bind cannot be minted by provider refresh or an old worker.
	if err := m.storage.PrepareQueuedReplacement(entry); err != nil {
		m.logger.Error().Err(err).Str("name", entry.Name).Msg("Failed to authorize completed download")
		entry.MarkAsError(err)
		_ = m.queue.Update(entry)
		return nil
	}

	// Now add entry to the main storage
	if err := m.AddOrUpdate(entry, func(t *storage.Entry) {
		m.RefreshEntries(true)
	}); err != nil {
		m.logger.Error().Err(err).Str("name", entry.Name).Msg("Failed to persist completed download")
		entry.MarkAsError(err)
		_ = m.queue.Update(entry)
		return nil
	}
	err := m.downloader.download(ctx, entry)
	if err != nil {
		if errors.Is(err, errDeleteQueueEntryOnJobFinish) {
			return err
		}
		m.logger.Error().
			Err(err).
			Str("name", entry.Name).
			Msg("Error running post-download action")
		entry.MarkAsError(err)
		_ = m.queue.Update(entry)
		return nil
	}
	return nil
}

// processTorrent handles the complete torrent lifecycle
func (m *Manager) processNewTorrent(ctx context.Context, torrent *storage.Entry, debridTorrent *debridTypes.Torrent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Update status to submitting
	torrent.UpdatedAt = time.Now()
	applyDebridTorrentToEntry(torrent, debridTorrent)
	if err := m.queue.Update(torrent); err != nil {
		return err
	}

	if debridTorrent.Status != debridTypes.TorrentStatusDownloaded {
		m.logger.Info().
			Str("debrid", debridTorrent.Debrid).
			Str("name", debridTorrent.Name).
			Msg("Started downloading torrent")
		return nil
	}

	return m.processAction(ctx, torrent)
}

func applyDebridTorrentToEntry(torrent *storage.Entry, debridTorrent *debridTypes.Torrent) {
	_ = torrent.AddTorrentProvider(debridTorrent)
	torrent.ActiveProvider = debridTorrent.Debrid
	torrent.Bytes = debridTorrent.GetSize()
	torrent.Size = debridTorrent.GetSize()
	torrent.Name = debridTorrent.Name
	torrent.OriginalFilename = debridTorrent.OriginalFilename
	torrent.UpdatedAt = time.Now()

	for _, file := range debridTorrent.Files {
		tFile := &storage.File{
			Name:      file.Name,
			Path:      file.Path,
			Size:      file.Size,
			ByteRange: file.ByteRange,
			Deleted:   file.Deleted,
			InfoHash:  torrent.InfoHash,
			AddedOn:   torrent.AddedOn,
		}
		torrent.Files[file.Name] = tFile
	}

	if debridTorrent.Status != debridTypes.TorrentStatusDownloaded {
		return
	}
	if placement := torrent.GetActiveProvider(); placement != nil {
		now := time.Now()
		placement.DownloadedAt = &now
		placement.Progress = 1.0
	}
}

// SendToDebrid submits a magnet to debrid service(s) - replaces debrid.Parse
func (m *Manager) SendToDebrid(ctx context.Context, importRequest *ImportRequest) (*debridTypes.Torrent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.validateTorrentImportRequest(importRequest); err != nil {
		return nil, err
	}
	clients := m.FilterDebrid(func(c common.Client) bool {
		if importRequest.SelectedDebrid != "" && c.Config().Name != importRequest.SelectedDebrid {
			return false
		}
		return true
	})

	if len(clients) == 0 {
		return nil, fmt.Errorf("no debrid clients available")
	}

	errs := make([]error, 0, len(clients))

	for _, db := range clients {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		providerConfig := db.Config()
		providerName := providerConfig.Name
		if providerName == "" {
			providerName = providerConfig.Provider
		}
		if providerName == "" {
			providerName = "unnamed provider"
		}
		_logger := db.Logger()
		if rejectionErr := m.cachedSubmissionRejection(providerName, importRequest.Magnet.InfoHash); rejectionErr != nil {
			_logger.Warn().
				Str("Provider", providerName).
				Str("Hash", importRequest.Magnet.InfoHash).
				Msg("Skipping provider during content-rejection cooldown")
			errs = append(errs, fmt.Errorf("%s submission: %w", providerName, rejectionErr))
			continue
		}
		overrideDownloadUncached := false

		if importRequest.DownloadUncached != nil {
			overrideDownloadUncached = *importRequest.DownloadUncached
		} else {
			overrideDownloadUncached = providerConfig.DownloadUncached
		}
		// Providers are allowed to populate the torrent in place. Construct each
		// fallback candidate from source fields so an earlier failed provider
		// cannot leak state into the next attempt. Do not copy Torrent: it owns
		// synchronization state used by its file map.
		debridTorrent := &debridTypes.Torrent{
			InfoHash:         importRequest.Magnet.InfoHash,
			Magnet:           importRequest.Magnet,
			Name:             importRequest.Magnet.Name,
			Arr:              importRequest.Arr,
			Size:             importRequest.Magnet.Size,
			Files:            make(map[string]debridTypes.File),
			DownloadUncached: overrideDownloadUncached,
		}
		_logger.Info().
			Str("Provider", providerName).
			Str("Arr", importRequest.Arr.Name).
			Str("Hash", debridTorrent.InfoHash).
			Str("Name", debridTorrent.Name).
			Str("Action", string(importRequest.Action)).
			Msg("Processing torrent")

		dbt, err := submitProviderMagnet(ctx, db, debridTorrent)
		if ctxErr := ctx.Err(); ctxErr != nil {
			if dbt != nil && dbt.Id != "" {
				ctxErr = errors.Join(ctxErr, m.deleteProviderTorrent(db, dbt.Id))
			}
			return nil, ctxErr
		}
		if err != nil || dbt == nil || dbt.Id == "" {
			if err == nil {
				err = fmt.Errorf("provider returned an incomplete submission response")
			} else {
				m.recordSubmissionRejection(
					providerName,
					importRequest.Magnet.InfoHash,
					importRequest.Magnet.Name,
					err,
				)
			}
			errs = append(errs, fmt.Errorf("%s submission: %w", providerName, err))
			continue
		}
		dbt.Arr = importRequest.Arr
		_logger.Info().Str("id", dbt.Id).Msgf("Entry: %s submitted to %s", dbt.Name, providerName)

		torrent, err := checkProviderStatus(ctx, db, dbt)
		if ctxErr := ctx.Err(); ctxErr != nil {
			rollbackID := dbt.Id
			if torrent != nil && torrent.Id != "" {
				rollbackID = torrent.Id
			}
			return nil, errors.Join(ctxErr, m.deleteProviderTorrent(db, rollbackID))
		}
		if err != nil {
			m.recordSubmissionRejection(
				providerName,
				importRequest.Magnet.InfoHash,
				importRequest.Magnet.Name,
				err,
			)
			rollbackID := dbt.Id
			if torrent != nil && torrent.Id != "" {
				rollbackID = torrent.Id
			}
			rollbackErr := m.deleteProviderTorrent(db, rollbackID)
			errs = append(errs, errors.Join(
				fmt.Errorf("%s status check: %w", providerName, err),
				rollbackErr,
			))
			continue
		}
		if torrent == nil {
			statusErr := fmt.Errorf("%s returned nil after checking torrent %s status", providerName, dbt.Name)
			rollbackErr := m.deleteProviderTorrent(db, dbt.Id)
			errs = append(errs, errors.Join(statusErr, rollbackErr))
			continue
		}
		if err := validateTorrentRootName(torrent.Name, false); err != nil {
			rollbackErr := m.deleteProviderTorrent(db, torrent.Id)
			errs = append(errs, errors.Join(
				fmt.Errorf("%s returned an unsafe torrent name: %w", providerName, err),
				rollbackErr,
			))
			continue
		}
		return torrent, nil
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("failed to process torrent: no clients available")
	}
	joinedErrors := errors.Join(errs...)
	return nil, fmt.Errorf("failed to process torrent: %w", joinedErrors)
}
