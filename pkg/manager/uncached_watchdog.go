package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const minimumUncachedStallTimeout = 10 * time.Minute

func stallableProviderState(state string) bool {
	state = strings.ToLower(strings.TrimSpace(strings.SplitN(state, "(", 2)[0]))
	switch state {
	case "":
		// Older providers do not expose a raw state. Preserve their watchdog
		// behavior, with the additional confirming status request.
		return true
	case "downloading", "stalled", "stalleddl":
		return true
	default:
		return false
	}
}

func parseUncachedStallTimeout(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	timeout, err := utils.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse uncached stall timeout: %w", err)
	}
	if timeout < minimumUncachedStallTimeout {
		return 0, fmt.Errorf(
			"uncached stall timeout must be at least %s",
			minimumUncachedStallTimeout,
		)
	}
	return timeout, nil
}

// uncachedTransferStalled is deliberately conservative. A transfer is eligible
// only when uncached downloading was actually used, the provider is still
// reporting an incomplete download, a successful poll has established a
// progress baseline, and the provider currently reports no transfer speed.
func uncachedTransferStalled(
	entry *storage.Entry,
	providerStatus debridTypes.TorrentStatus,
	now time.Time,
	timeout time.Duration,
) bool {
	if entry == nil || timeout <= 0 || !entry.IsTorrent() || !entry.DownloadUncached {
		return false
	}
	if providerStatus != debridTypes.TorrentStatusDownloading ||
		entry.State != storage.EntryStateDownloading ||
		entry.Status != debridTypes.TorrentStatusDownloading {
		return false
	}
	if entry.Progress >= 1 || entry.Speed > 0 || entry.LastObservedAt == nil || entry.LastProgressAt == nil {
		return false
	}
	return !now.Before(entry.LastProgressAt.Add(timeout))
}

// handoffUncachedFailures asks the owning Arr to remove exactly one confirmed
// failed download, blocklist that release, and re-search. It runs outside the per-entry
// worker lease: the Arr synchronously calls Decypharr's qBittorrent delete API,
// which can then drain the old worker and durably clean the provider placement.
// A persisted stalledDL row makes the handoff restart-safe and naturally
// retryable until the Arr acknowledges it.
func (m *Manager) handoffUncachedFailures(ctx context.Context) error {
	if m == nil || m.queue == nil || m.arr == nil {
		return nil
	}
	entries := m.queue.ListFilter(
		"",
		config.ProtocolTorrent,
		storage.EntryStateStalledDL,
		nil,
		"added_on",
		false,
	)
	var handoffErr error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(handoffErr, err)
		}
		if entry == nil || !entry.DownloadUncached {
			continue
		}
		owner := m.arr.Get(entry.Category)
		if owner == nil {
			m.logger.Debug().
				Str("entry", entry.InfoHash).
				Str("category", entry.Category).
				Msg("Failed uncached transfer has no configured Arr owner; leaving it for manual review")
			continue
		}
		var handedOff bool
		var err error
		if entry.ClientEndpoint == "" {
			m.logger.Warn().Str("entry", entry.InfoHash).Msg("Uncached transfer lacks submitting client evidence; manual Arr review required")
			continue
		}
		handedOff, err = owner.BlocklistAndResearchDownloadForEndpointCtx(ctx, entry.InfoHash, entry.ClientEndpoint)
		if err != nil {
			m.uncachedHandoffErrors.Add(1)
			handoffErr = errors.Join(
				handoffErr,
				fmt.Errorf("handoff failed transfer %s to %s: %w", entry.InfoHash, owner.Name, err),
			)
			continue
		}
		if !handedOff {
			m.logger.Debug().
				Str("entry", entry.InfoHash).
				Str("arr", owner.Name).
				Msg("Failed uncached transfer is not yet present in the Arr queue")
			continue
		}
		m.uncachedHandoffAccepted.Add(1)
		m.logger.Info().
			Str("entry", entry.InfoHash).
			Str("arr", owner.Name).
			Str("reason", entry.HandoffReason).
			Msg("Arr accepted failed uncached transfer for blocklist and replacement search")
	}
	return handoffErr
}
