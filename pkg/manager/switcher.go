package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// This is in-charge of moving torrents between different debrid services

// SwitchTorrent moves a torrent from one debrid to another
func (m *Manager) SwitchTorrent(ctx context.Context, infohash, target string, keepOld, waitComplete bool) (*storage.SwitcherJob, error) {
	// GetReader the entry
	entry, err := m.GetEntry(infohash)
	if err != nil {
		return nil, fmt.Errorf("failed to get entry: %w", err)
	}

	// Check if already on target debrid
	if entry.ActiveProvider == target {
		return nil, storage.ErrAlreadyOnDebrid
	}

	// Need to actually migrate - create job
	job := &storage.SwitcherJob{
		ID:             uuid.New().String(),
		InfoHash:       infohash,
		SourceProvider: entry.ActiveProvider,
		TargetProvider: target,
		Status:         storage.SwitcherStatusPending,
		Progress:       0,
		CreatedAt:      time.Now(),
		KeepOld:        keepOld,
		WaitComplete:   waitComplete,
	}

	// Store job
	m.migrationJobs.Store(job.ID, job)

	// Start migration in background
	if !m.startBackground("torrent migration", func() {
		m.executeMigration(job, entry)
	}) {
		m.migrationJobs.Delete(job.ID)
		return nil, fmt.Errorf("cannot switch torrent while manager is stopping")
	}

	return job, nil
}

// executeMigration performs the actual torrent migration - COMPLETE IMPLEMENTATION
func (m *Manager) executeMigration(job *storage.SwitcherJob, torrent *storage.Entry) {
	m.logger.Info().
		Str("job_id", job.ID).
		Str("torrent", torrent.Name).
		Str("source", job.SourceProvider).
		Str("target", job.TargetProvider).
		Msg("Starting torrent migration")
	job.Status = storage.SwitcherStatusInProgress

	// Verify the target client still exists before starting provider work.
	if m.ProviderClient(job.TargetProvider) == nil {
		job.Status = storage.SwitcherStatusFailed
		job.Error = fmt.Sprintf("target debrid %s not found", job.TargetProvider)
		job.CompletedAt = new(time.Now())
		return
	}
	// Submit to target debrid

	job.Progress = 10

	success, err := m.fixer.MoveTorrent(torrent, job.TargetProvider, false) // false = don't force re-download

	if err != nil || !success {
		job.Status = storage.SwitcherStatusFailed
		job.Error = fmt.Sprintf("failed to move torrent to target debrid: %v", err)
		job.CompletedAt = new(time.Now())
		m.logger.Error().
			Err(err).
			Str("job_id", job.ID).
			Msg("Failed to move torrent to target debrid")
		return
	}

	// MoveTorrent has durably activated the target. Source cleanup is a separate
	// phase owned by the source provider client and controlled by keep-old.
	if !job.KeepOld {
		sourcePlacement := torrent.Providers[job.SourceProvider]
		if sourcePlacement == nil {
			// Retain compatibility with older rows whose map key did not match
			// the configured provider name.
			for _, placement := range torrent.Providers {
				if placement != nil && placement.Provider == job.SourceProvider {
					sourcePlacement = placement
					break
				}
			}
		}

		if sourcePlacement != nil {
			sourceClient := m.ProviderClient(job.SourceProvider)
			if sourceClient == nil {
				job.Status = storage.SwitcherStatusFailed
				job.Error = fmt.Sprintf("source debrid %s not found after target activation", job.SourceProvider)
				job.CompletedAt = new(time.Now())
				m.logger.Error().
					Str("job_id", job.ID).
					Str("source", job.SourceProvider).
					Msg("Target activated, but source client is unavailable")
				return
			}
			if err := m.deleteProviderTorrent(sourceClient, sourcePlacement.ID); err != nil {
				job.Status = storage.SwitcherStatusFailed
				job.Error = fmt.Sprintf("failed to remove source placement %s/%s: %v", job.SourceProvider, sourcePlacement.ID, err)
				job.CompletedAt = new(time.Now())
				m.logger.Error().
					Err(err).
					Str("job_id", job.ID).
					Str("source", job.SourceProvider).
					Str("torrent_id", sourcePlacement.ID).
					Msg("Target activated, but source cleanup failed")
				return
			}
			torrent.RemoveProvider(job.SourceProvider)
		}
	}

	// Save updated torrent
	if err := m.AddOrUpdate(torrent, func(t *storage.Entry) {
		m.RefreshEntries(false)
	}); err != nil {
		job.Status = storage.SwitcherStatusFailed
		job.Error = fmt.Sprintf("failed to update torrent: %v", err)
		m.logger.Error().Err(err).Msg("Failed to update torrent after migration")
	} else {
		job.Status = storage.SwitcherStatusCompleted
		job.Progress = 100
	}

	job.CompletedAt = new(time.Now())

	m.logger.Info().
		Str("job_id", job.ID).
		Str("status", string(job.Status)).
		Msg("Migration completed")
}
