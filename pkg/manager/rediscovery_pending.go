package manager

import (
	"time"
)

// rediscoveryPendingLogInterval bounds how often the manager repeats the
// "awaiting provider absence" notice for the same entry. Provider sync would
// otherwise re-log the same guard rejection on every refresh cycle for every
// entry that legitimately cannot be rediscovered yet (deleted upstream, still
// returned by a stale provider list), which has produced thousands of
// duplicate ERROR lines per day per host.
const rediscoveryPendingLogInterval = time.Hour

// rediscoveryPendingNow is the clock seam for the log-rate tests; production
// always uses time.Now.
func (m *Manager) rediscoveryPendingNow() time.Time {
	if m.rediscoveryPendingNowFunc != nil {
		return m.rediscoveryPendingNowFunc()
	}
	return time.Now()
}

// shouldLogRediscoveryPending decides, under the limiter's mutex, whether this
// key is due for its once-per-interval operational notice. Callers log at
// Warn when it returns true and at Debug otherwise, so repeated syncs stay
// visible in debug runs without restorming production logs.
func (m *Manager) shouldLogRediscoveryPending(key string, now time.Time) bool {
	m.rediscoveryPendingMu.Lock()
	defer m.rediscoveryPendingMu.Unlock()
	if m.rediscoveryPendingLast == nil {
		m.rediscoveryPendingLast = make(map[string]time.Time)
	}
	last, seen := m.rediscoveryPendingLast[key]
	if seen && now.Sub(last) < rediscoveryPendingLogInterval {
		return false
	}
	m.rediscoveryPendingLast[key] = now
	return true
}

// noteRediscoveryPending emits the coalesced notice for a provider-sync skip.
func (m *Manager) noteRediscoveryPending(provider, infoHash string) {
	key := provider + "/" + infoHash
	if m.shouldLogRediscoveryPending(key, m.rediscoveryPendingNow()) {
		m.logger.Warn().
			Str("debrid", provider).
			Str("infohash", infoHash).
			Msg("Skipping rediscovery for deleted entry: awaiting authoritative post-delete provider absence")
		return
	}
	m.logger.Debug().
		Str("debrid", provider).
		Str("infohash", infoHash).
		Msg("Still awaiting post-delete provider absence; skipping rediscovery attempt")
}
