package vfs

import (
	"errors"
	"sync"

	"github.com/sirrobot01/decypharr/internal/utils"
)

var ErrCacheDiskLimit = errors.New("dfs cache: disk quota exhausted")

// diskQuota is the authoritative process-lifetime accounting for the DFS
// cache directory. Unlike buffer.Pool accounting, used bytes remain charged
// when an idle buffer closes because its sparse cache file persists on disk.
// Reservations are included in admission so concurrent writers cannot all
// observe the same remaining capacity.
type diskQuota struct {
	mu       sync.Mutex
	limit    int64
	used     int64
	reserved int64
}

type diskQuotaSnapshot struct {
	Limit     int64
	Used      int64
	Reserved  int64
	Available int64
}

func newDiskQuota(limit int64) *diskQuota {
	if limit < 0 {
		limit = 0
	}
	return &diskQuota{limit: limit}
}

// initialize is used once, before NewCache publishes the cache to readers.
func (q *diskQuota) initialize(used int64) {
	if used < 0 {
		used = 0
	}
	q.mu.Lock()
	q.used = used
	q.reserved = 0
	q.mu.Unlock()
}

func (q *diskQuota) tryReserve(n int64) bool {
	if q == nil || n <= 0 {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.limit > 0 && (n > q.limit || q.used > q.limit-n-q.reserved) {
		return false
	}
	q.reserved += n
	return true
}

// commit converts a reservation into persistent cache usage. committed may be
// smaller than reserved when a caller deliberately accepts a partial write.
func (q *diskQuota) commit(reserved, committed int64) {
	if q == nil || reserved <= 0 {
		return
	}
	if committed < 0 {
		committed = 0
	}
	if committed > reserved {
		committed = reserved
	}
	q.mu.Lock()
	q.reserved -= min(reserved, q.reserved)
	q.used += committed
	q.mu.Unlock()
}

func (q *diskQuota) cancel(reserved int64) {
	if q == nil || reserved <= 0 {
		return
	}
	q.mu.Lock()
	q.reserved -= min(reserved, q.reserved)
	q.mu.Unlock()
}

// release records physical cache bytes that were actually deleted or
// hole-punched. Closing a persistent buffer intentionally does not call this.
func (q *diskQuota) release(n int64) int64 {
	if q == nil || n <= 0 {
		return 0
	}
	q.mu.Lock()
	released := min(n, q.used)
	q.used -= released
	q.mu.Unlock()
	return released
}

func (q *diskQuota) shortfall(request int64) int64 {
	if q == nil || request <= 0 {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.limit <= 0 {
		return 0
	}
	return max(q.used+q.reserved+request-q.limit, 0)
}

func (q *diskQuota) snapshot() diskQuotaSnapshot {
	if q == nil {
		return diskQuotaSnapshot{}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	available := int64(0)
	if q.limit > 0 {
		available = max(q.limit-q.used-q.reserved, 0)
	}
	return diskQuotaSnapshot{
		Limit:     q.limit,
		Used:      q.used,
		Reserved:  q.reserved,
		Available: available,
	}
}

func (c *Cache) reserveDisk(excludedKey string, n int64) bool {
	if n <= 0 || c == nil || c.quota == nil {
		return true
	}
	if c.quota.tryReserve(n) {
		return true
	}
	c.quotaPressure.Add(1)
	c.reclaimForReservation(excludedKey, n)
	if c.quota.tryReserve(n) {
		return true
	}
	c.quotaDenials.Add(1)
	return false
}

func (c *Cache) releaseDisk(n int64) int64 {
	if c == nil || c.quota == nil || n <= 0 {
		return 0
	}
	released := c.quota.release(n)
	if released > 0 {
		c.quotaReclaimed.Add(released)
	}
	return released
}

// reclaimForReservation synchronously frees safe active read-behind ranges,
// then cold persistent entries. It never closes the item performing the write.
// cleanupMu serializes this path with periodic cleanup and other pressure
// writers, preventing a thundering herd of directory scans and removals.
func (c *Cache) reclaimForReservation(excludedKey string, request int64) {
	if c == nil || c.quota == nil || request <= 0 {
		return
	}

	c.cleanupMu.Lock()
	defer c.cleanupMu.Unlock()

	shortfall := c.quota.shortfall(request)
	if shortfall <= 0 {
		return
	}
	if c.pool != nil {
		// Fence hole-punch callbacks out of a writer's interval between its
		// buffer publish and metadata/quota commit. Do not hold this while
		// closing idle items: Close waits for their per-item writer lock.
		c.diskOpMu.Lock()
		c.pool.ReclaimDisk(shortfall)
		c.diskOpMu.Unlock()
		shortfall = c.quota.shortfall(request)
		if shortfall <= 0 {
			return
		}
	}

	// Convert idle in-memory items to closed persistent candidates, excluding
	// the writer that triggered pressure, then remove the coldest candidates
	// until the reservation fits or nothing safe remains.
	c.cleanupItemsExcept(utils.Now(), true, excludedKey)
	scan := c.scanDiskCandidates()
	target := max(scan.totalSize-shortfall, 0)
	remaining, _, removalErrors, removed := c.evictCandidates(
		utils.Now(),
		scan.candidates,
		scan.totalSize,
		target,
	)
	scan.errors += removalErrors
	c.releaseDisk(scan.totalSize - remaining)
	c.storeDiskStats(scan.candidates, removed)
}
