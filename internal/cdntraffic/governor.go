package cdntraffic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultConcurrentRequests = 8
	defaultTorBoxRequests     = 4
	defaultMinimumRequests    = 1
	defaultRecoveryInterval   = 30 * time.Second
	defaultThrottleBackoff    = time.Second
	defaultMaximumBackoff     = 5 * time.Minute
	defaultInteractiveBurst   = 8
	maxTrafficStates          = 1024
)

// Options controls the governor. Zero values select production defaults.
// The fields are exported primarily so deterministic tests can use short
// recovery windows; Decypharr intentionally does not expose user settings yet.
type Options struct {
	DefaultLimit        int
	TorBoxLimit         int
	MinimumLimit        int
	RecoveryInterval    time.Duration
	DefaultBackoff      time.Duration
	MaximumBackoff      time.Duration
	MaxInteractiveBurst int
}

type waiter struct {
	priority Priority
	ready    chan struct{}
	granted  bool
	queuedAt time.Time
}

type trafficState struct {
	provider     string
	providerType string
	host         string

	limit    int
	maxLimit int
	minLimit int

	activeInteractive  int
	activeBackground   int
	waitingInteractive []*waiter
	waitingBackground  []*waiter
	interactiveBurst   int

	blockedUntil  time.Time
	recoverAfter  time.Time
	wakeScheduled bool

	admittedInteractive uint64
	admittedBackground  uint64
	throttles           uint64
	waitNanos           uint64
	lastUsed            time.Time
}

// ProviderStats is a secret-free aggregate for one configured provider.
type ProviderStats struct {
	Provider             string     `json:"provider"`
	ProviderType         string     `json:"provider_type,omitempty"`
	Accounts             int        `json:"accounts"`
	Hosts                []string   `json:"hosts,omitempty"`
	Active               int        `json:"active"`
	ActiveInteractive    int        `json:"active_interactive"`
	ActiveBackground     int        `json:"active_background"`
	WaitingInteractive   int        `json:"waiting_interactive"`
	WaitingBackground    int        `json:"waiting_background"`
	CurrentLimit         int        `json:"current_limit"`
	MaximumLimit         int        `json:"maximum_limit"`
	AdmittedInteractive  uint64     `json:"admitted_interactive"`
	AdmittedBackground   uint64     `json:"admitted_background"`
	Throttles            uint64     `json:"throttles"`
	CumulativeWaitMillis uint64     `json:"cumulative_wait_ms"`
	BlockedUntil         *time.Time `json:"blocked_until,omitempty"`
}

// Stats is a point-in-time view of CDN admission and throttling.
type Stats struct {
	Active             int             `json:"active"`
	WaitingInteractive int             `json:"waiting_interactive"`
	WaitingBackground  int             `json:"waiting_background"`
	Throttles          uint64          `json:"throttles"`
	Providers          []ProviderStats `json:"providers"`
}

// Governor coordinates logical CDN requests across every shared HTTP client.
// Its mutex is touched only at request admission/release, never while bytes are
// copied, so it does not sit on the streaming hot path.
type Governor struct {
	mu      sync.Mutex
	states  map[string]*trafficState
	options Options
	now     func() time.Time
}

// Permit owns one active request slot.
type Permit struct {
	once    sync.Once
	release func()
}

// Release returns the request slot exactly once.
func (p *Permit) Release() {
	if p == nil {
		return
	}
	p.once.Do(p.release)
}

// New creates a provider-aware adaptive CDN governor.
func New(options Options) *Governor {
	if options.DefaultLimit <= 0 {
		options.DefaultLimit = defaultConcurrentRequests
	}
	if options.TorBoxLimit <= 0 {
		options.TorBoxLimit = defaultTorBoxRequests
	}
	if options.MinimumLimit <= 0 {
		options.MinimumLimit = defaultMinimumRequests
	}
	if options.RecoveryInterval <= 0 {
		options.RecoveryInterval = defaultRecoveryInterval
	}
	if options.DefaultBackoff <= 0 {
		options.DefaultBackoff = defaultThrottleBackoff
	}
	if options.MaximumBackoff <= 0 {
		options.MaximumBackoff = defaultMaximumBackoff
	}
	if options.MaxInteractiveBurst <= 0 {
		options.MaxInteractiveBurst = defaultInteractiveBurst
	}
	return &Governor{
		states:  make(map[string]*trafficState),
		options: options,
		now:     time.Now,
	}
}

// Acquire waits for a provider/account slot. Interactive requests retain one
// slot ahead of bulk work whenever the current limit is at least two.
func (g *Governor) Acquire(ctx context.Context, identity Identity, requestHost string, priority Priority) (*Permit, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	g.mu.Lock()
	key, state := g.stateLocked(identity, requestHost)
	w := &waiter{priority: priority, ready: make(chan struct{}), queuedAt: g.now()}
	if priority == PriorityInteractive {
		state.waitingInteractive = append(state.waitingInteractive, w)
	} else {
		state.waitingBackground = append(state.waitingBackground, w)
	}
	g.dispatchLocked(key, state)
	g.mu.Unlock()

	select {
	case <-w.ready:
		if err := ctx.Err(); err != nil {
			g.release(key, priority)
			return nil, err
		}
		return &Permit{release: func() { g.release(key, priority) }}, nil
	case <-ctx.Done():
		g.mu.Lock()
		if w.granted {
			g.releaseLocked(key, state, priority)
		} else {
			g.removeWaiterLocked(state, w)
		}
		g.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Observe adjusts admission after a completed response header. Only 429
// reduces concurrency; successful responses restore one slot at a time.
func (g *Governor) Observe(identity Identity, requestHost string, statusCode int, header http.Header) {
	g.mu.Lock()
	key, state := g.stateLocked(identity, requestHost)
	now := g.now()
	if statusCode == http.StatusTooManyRequests {
		state.throttles++
		if state.limit > state.minLimit {
			state.limit = max(state.minLimit, (state.limit+1)/2)
		}
		backoff := retryAfter(header, now)
		if backoff <= 0 {
			backoff = g.options.DefaultBackoff
		}
		if backoff > g.options.MaximumBackoff {
			backoff = g.options.MaximumBackoff
		}
		blockedUntil := now.Add(backoff)
		if blockedUntil.After(state.blockedUntil) {
			state.blockedUntil = blockedUntil
		}
		state.recoverAfter = state.blockedUntil.Add(g.options.RecoveryInterval)
		g.scheduleWakeLocked(key, state, now)
	} else if statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices &&
		state.limit < state.maxLimit && !now.Before(state.recoverAfter) {
		state.limit++
		state.recoverAfter = now.Add(g.options.RecoveryInterval)
	}
	g.dispatchLocked(key, state)
	g.mu.Unlock()
}

func (g *Governor) release(key string, priority Priority) {
	g.mu.Lock()
	state := g.states[key]
	if state != nil {
		g.releaseLocked(key, state, priority)
	}
	g.mu.Unlock()
}

func (g *Governor) releaseLocked(key string, state *trafficState, priority Priority) {
	if priority == PriorityInteractive {
		if state.activeInteractive > 0 {
			state.activeInteractive--
		}
	} else if state.activeBackground > 0 {
		state.activeBackground--
	}
	g.dispatchLocked(key, state)
}

func (g *Governor) dispatchLocked(key string, state *trafficState) {
	now := g.now()
	if now.Before(state.blockedUntil) {
		g.scheduleWakeLocked(key, state, now)
		return
	}
	state.blockedUntil = time.Time{}

	for state.activeInteractive+state.activeBackground < state.limit {
		var w *waiter
		if state.activeBackground >= backgroundCapacity(state.limit) {
			if len(state.waitingInteractive) == 0 {
				return
			}
			w = state.waitingInteractive[0]
			state.waitingInteractive = state.waitingInteractive[1:]
			state.interactiveBurst++
		} else {
			w = g.nextWaiterLocked(state)
		}
		if w == nil {
			return
		}
		w.granted = true
		waited := now.Sub(w.queuedAt)
		if waited > 0 {
			state.waitNanos += uint64(waited)
		}
		if w.priority == PriorityInteractive {
			state.activeInteractive++
			state.admittedInteractive++
		} else {
			state.activeBackground++
			state.admittedBackground++
		}
		close(w.ready)
	}
}

func (g *Governor) nextWaiterLocked(state *trafficState) *waiter {
	if len(state.waitingInteractive) > 0 && len(state.waitingBackground) == 0 {
		// Playback that ran while no bulk work was waiting does not consume the
		// fairness budget of a future background request.
		state.interactiveBurst = 0
		w := state.waitingInteractive[0]
		state.waitingInteractive = state.waitingInteractive[1:]
		return w
	}
	if len(state.waitingInteractive) > 0 && state.interactiveBurst < g.options.MaxInteractiveBurst {
		w := state.waitingInteractive[0]
		state.waitingInteractive = state.waitingInteractive[1:]
		state.interactiveBurst++
		return w
	}
	if len(state.waitingBackground) > 0 {
		w := state.waitingBackground[0]
		state.waitingBackground = state.waitingBackground[1:]
		state.interactiveBurst = 0
		return w
	}
	if len(state.waitingInteractive) > 0 {
		w := state.waitingInteractive[0]
		state.waitingInteractive = state.waitingInteractive[1:]
		state.interactiveBurst++
		return w
	}
	return nil
}

func (g *Governor) removeWaiterLocked(state *trafficState, target *waiter) {
	queue := &state.waitingBackground
	if target.priority == PriorityInteractive {
		queue = &state.waitingInteractive
	}
	for i, candidate := range *queue {
		if candidate == target {
			copy((*queue)[i:], (*queue)[i+1:])
			*queue = (*queue)[:len(*queue)-1]
			return
		}
	}
}

func (g *Governor) scheduleWakeLocked(key string, state *trafficState, now time.Time) {
	if state.wakeScheduled || !now.Before(state.blockedUntil) {
		return
	}
	state.wakeScheduled = true
	delay := state.blockedUntil.Sub(now)
	time.AfterFunc(delay, func() {
		g.mu.Lock()
		current := g.states[key]
		if current == state {
			state.wakeScheduled = false
			g.dispatchLocked(key, state)
		}
		g.mu.Unlock()
	})
}

func (g *Governor) stateLocked(identity Identity, requestHost string) (string, *trafficState) {
	now := g.now()
	provider := strings.TrimSpace(identity.Provider)
	providerType := strings.ToLower(strings.TrimSpace(identity.ProviderType))
	if provider == "" {
		provider = providerType
	}
	host := strings.ToLower(strings.TrimSpace(requestHost))
	key := stateKey(provider, identity.AccountToken, host)
	state := g.states[key]
	maxLimit := g.limitForProvider(providerType)
	if state == nil {
		if len(g.states) >= maxTrafficStates {
			g.pruneOldestIdleStateLocked()
		}
		minimum := min(g.options.MinimumLimit, maxLimit)
		state = &trafficState{
			provider:     provider,
			providerType: providerType,
			host:         host,
			limit:        maxLimit,
			maxLimit:     maxLimit,
			minLimit:     minimum,
			lastUsed:     now,
		}
		g.states[key] = state
		return key, state
	}
	if state.provider == "" && provider != "" {
		state.provider = provider
	}
	if state.providerType == "" && providerType != "" {
		state.providerType = providerType
		state.maxLimit = maxLimit
		state.minLimit = min(g.options.MinimumLimit, maxLimit)
		state.limit = min(state.limit, state.maxLimit)
	}
	if host != "" {
		state.host = host
	}
	state.lastUsed = now
	return key, state
}

func (g *Governor) pruneOldestIdleStateLocked() {
	var oldestKey string
	var oldest time.Time
	for key, state := range g.states {
		if state.activeInteractive != 0 || state.activeBackground != 0 ||
			len(state.waitingInteractive) != 0 || len(state.waitingBackground) != 0 ||
			state.wakeScheduled {
			continue
		}
		if oldestKey == "" || state.lastUsed.Before(oldest) {
			oldestKey = key
			oldest = state.lastUsed
		}
	}
	if oldestKey != "" {
		delete(g.states, oldestKey)
	}
}

func (g *Governor) limitForProvider(providerType string) int {
	if strings.EqualFold(providerType, "torbox") {
		return max(1, g.options.TorBoxLimit)
	}
	return max(1, g.options.DefaultLimit)
}

func stateKey(provider, token, host string) string {
	if provider == "" {
		return "host\x00" + host
	}
	account := "shared"
	if token != "" {
		digest := sha256.Sum256([]byte(token))
		account = hex.EncodeToString(digest[:8])
	}
	return strings.ToLower(provider) + "\x00" + account
}

func backgroundCapacity(limit int) int {
	if limit <= 1 {
		return 1
	}
	return limit - 1
}

func retryAfter(header http.Header, now time.Time) time.Duration {
	if header == nil {
		return 0
	}
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

// Snapshot returns secret-free provider aggregates in deterministic order.
func (g *Governor) Snapshot() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()

	type aggregate struct {
		stats ProviderStats
		hosts map[string]struct{}
	}
	aggregates := make(map[string]*aggregate)
	result := Stats{}
	for _, state := range g.states {
		provider := state.provider
		if provider == "" {
			provider = state.host
		}
		key := strings.ToLower(provider) + "\x00" + state.providerType
		a := aggregates[key]
		if a == nil {
			a = &aggregate{
				stats: ProviderStats{Provider: provider, ProviderType: state.providerType},
				hosts: make(map[string]struct{}),
			}
			aggregates[key] = a
		}
		a.stats.Accounts++
		if state.host != "" {
			a.hosts[state.host] = struct{}{}
		}
		a.stats.ActiveInteractive += state.activeInteractive
		a.stats.ActiveBackground += state.activeBackground
		a.stats.WaitingInteractive += len(state.waitingInteractive)
		a.stats.WaitingBackground += len(state.waitingBackground)
		a.stats.CurrentLimit += state.limit
		a.stats.MaximumLimit += state.maxLimit
		a.stats.AdmittedInteractive += state.admittedInteractive
		a.stats.AdmittedBackground += state.admittedBackground
		a.stats.Throttles += state.throttles
		a.stats.CumulativeWaitMillis += state.waitNanos / uint64(time.Millisecond)
		if !state.blockedUntil.IsZero() &&
			(a.stats.BlockedUntil == nil || state.blockedUntil.After(*a.stats.BlockedUntil)) {
			blockedUntil := state.blockedUntil
			a.stats.BlockedUntil = &blockedUntil
		}
	}
	for _, a := range aggregates {
		a.stats.Active = a.stats.ActiveInteractive + a.stats.ActiveBackground
		for host := range a.hosts {
			a.stats.Hosts = append(a.stats.Hosts, host)
		}
		sort.Strings(a.stats.Hosts)
		result.Active += a.stats.Active
		result.WaitingInteractive += a.stats.WaitingInteractive
		result.WaitingBackground += a.stats.WaitingBackground
		result.Throttles += a.stats.Throttles
		result.Providers = append(result.Providers, a.stats)
	}
	sort.Slice(result.Providers, func(i, j int) bool {
		if result.Providers[i].Provider == result.Providers[j].Provider {
			return result.Providers[i].ProviderType < result.Providers[j].ProviderType
		}
		return result.Providers[i].Provider < result.Providers[j].Provider
	})
	return result
}
