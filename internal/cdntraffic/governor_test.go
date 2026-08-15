package cdntraffic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testGovernor(limit int) *Governor {
	return New(Options{
		DefaultLimit:        limit,
		TorBoxLimit:         limit,
		MinimumLimit:        1,
		RecoveryInterval:    20 * time.Millisecond,
		DefaultBackoff:      40 * time.Millisecond,
		MaximumBackoff:      time.Second,
		MaxInteractiveBurst: 4,
	})
}

func mustAcquire(t *testing.T, governor *Governor, identity Identity, priority Priority) *Permit {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	permit, err := governor.Acquire(ctx, identity, "cdn.example", priority)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	return permit
}

func TestGovernorReservesCapacityForInteractiveRequests(t *testing.T) {
	governor := testGovernor(3)
	identity := Identity{Provider: "debrid", ProviderType: "realdebrid", AccountToken: "secret"}

	backgroundOne := mustAcquire(t, governor, identity, PriorityBackground)
	backgroundTwo := mustAcquire(t, governor, identity, PriorityBackground)

	thirdBackground := make(chan *Permit, 1)
	go func() {
		permit, _ := governor.Acquire(context.Background(), identity, "cdn.example", PriorityBackground)
		thirdBackground <- permit
	}()
	select {
	case permit := <-thirdBackground:
		permit.Release()
		t.Fatal("third background request consumed the playback reserve")
	case <-time.After(20 * time.Millisecond):
	}

	interactive := mustAcquire(t, governor, identity, PriorityInteractive)
	interactive.Release()
	select {
	case permit := <-thirdBackground:
		permit.Release()
		t.Fatal("background request was admitted while the reserved slot was the only capacity")
	case <-time.After(20 * time.Millisecond):
	}

	backgroundOne.Release()
	select {
	case permit := <-thirdBackground:
		if permit == nil {
			t.Fatal("queued background request returned a nil permit")
		}
		permit.Release()
	case <-time.After(time.Second):
		t.Fatal("queued background request was not admitted after background capacity returned")
	}
	backgroundTwo.Release()
}

func TestGovernorQueuesInteractiveAheadOfBackground(t *testing.T) {
	governor := testGovernor(2)
	identity := Identity{Provider: "debrid", AccountToken: "secret"}
	first := mustAcquire(t, governor, identity, PriorityInteractive)
	second := mustAcquire(t, governor, identity, PriorityInteractive)

	backgroundReady := make(chan *Permit, 1)
	interactiveReady := make(chan *Permit, 1)
	go func() {
		permit, _ := governor.Acquire(context.Background(), identity, "cdn.example", PriorityBackground)
		backgroundReady <- permit
	}()
	go func() {
		permit, _ := governor.Acquire(context.Background(), identity, "cdn.example", PriorityInteractive)
		interactiveReady <- permit
	}()

	deadline := time.Now().Add(time.Second)
	for governor.Snapshot().WaitingInteractive != 1 || governor.Snapshot().WaitingBackground != 1 {
		if time.Now().After(deadline) {
			t.Fatal("requests did not enter both priority queues")
		}
		time.Sleep(time.Millisecond)
	}

	first.Release()
	var queuedInteractive *Permit
	select {
	case queuedInteractive = <-interactiveReady:
		if queuedInteractive == nil {
			t.Fatal("queued interactive request returned a nil permit")
		}
	case permit := <-backgroundReady:
		permit.Release()
		t.Fatal("background request was admitted ahead of queued playback")
	case <-time.After(time.Second):
		t.Fatal("queued interactive request was not admitted")
	}

	queuedInteractive.Release()
	second.Release()
	select {
	case permit := <-backgroundReady:
		if permit == nil {
			t.Fatal("queued background request returned a nil permit")
		}
		permit.Release()
	case <-time.After(time.Second):
		t.Fatal("queued background request starved after playback drained")
	}
}

func TestGovernorCancellationRemovesWaiter(t *testing.T) {
	governor := testGovernor(1)
	identity := Identity{Provider: "debrid"}
	active := mustAcquire(t, governor, identity, PriorityBackground)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := governor.Acquire(ctx, identity, "cdn.example", PriorityInteractive)
		result <- err
	}()

	deadline := time.Now().Add(time.Second)
	for governor.Snapshot().WaitingInteractive != 1 {
		if time.Now().After(deadline) {
			t.Fatal("interactive request did not enter the queue")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire() cancellation error = %v", err)
	}
	if got := governor.Snapshot().WaitingInteractive; got != 0 {
		t.Fatalf("waiting interactive = %d, want 0", got)
	}
	active.Release()
}

func TestGovernorBacksOffOn429AndRecoversGradually(t *testing.T) {
	governor := testGovernor(4)
	identity := Identity{Provider: "debrid", ProviderType: "realdebrid", AccountToken: "secret"}
	governor.Observe(identity, "cdn.example", http.StatusTooManyRequests, nil)

	stats := governor.Snapshot()
	if len(stats.Providers) != 1 {
		t.Fatalf("provider stats = %d, want 1", len(stats.Providers))
	}
	if stats.Providers[0].CurrentLimit != 2 {
		t.Fatalf("current limit = %d, want 2", stats.Providers[0].CurrentLimit)
	}
	if stats.Throttles != 1 || stats.Providers[0].BlockedUntil == nil || stats.Providers[0].BlockedUntil.IsZero() {
		t.Fatalf("throttle snapshot = %+v", stats)
	}

	started := time.Now()
	permit := mustAcquire(t, governor, identity, PriorityInteractive)
	if waited := time.Since(started); waited < 25*time.Millisecond {
		permit.Release()
		t.Fatalf("429 admission wait = %s, want at least 25ms", waited)
	}
	permit.Release()

	time.Sleep(25 * time.Millisecond)
	governor.Observe(identity, "cdn.example", http.StatusOK, nil)
	if got := governor.Snapshot().Providers[0].CurrentLimit; got != 3 {
		t.Fatalf("recovered limit = %d, want 3", got)
	}
}

func TestGovernorSnapshotDoesNotExposeAccountToken(t *testing.T) {
	governor := testGovernor(2)
	identity := Identity{Provider: "named-provider", ProviderType: "torbox", AccountToken: "do-not-expose"}
	permit := mustAcquire(t, governor, identity, PriorityInteractive)
	permit.Release()

	encoded, err := json.Marshal(governor.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), identity.AccountToken) {
		t.Fatalf("snapshot exposed account token: %s", encoded)
	}
	if strings.Contains(string(encoded), "blocked_until") {
		t.Fatalf("healthy snapshot included an empty backoff timestamp: %s", encoded)
	}
}

func TestGovernorUsesTorBoxSafeStartingLimitForNamedProvider(t *testing.T) {
	governor := New(Options{})
	governor.Observe(
		Identity{Provider: "my-primary", ProviderType: "torbox", AccountToken: "secret"},
		"api.torbox.app",
		http.StatusOK,
		nil,
	)
	stats := governor.Snapshot()
	if len(stats.Providers) != 1 {
		t.Fatalf("provider stats = %d, want 1", len(stats.Providers))
	}
	if got := stats.Providers[0].MaximumLimit; got != defaultTorBoxRequests {
		t.Fatalf("TorBox maximum limit = %d, want %d", got, defaultTorBoxRequests)
	}
}

func TestGovernorConcurrentAcquireReleaseStaysWithinLimit(t *testing.T) {
	governor := testGovernor(4)
	identity := Identity{Provider: "debrid", AccountToken: "secret"}
	var active atomic.Int64
	var peak atomic.Int64
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			priority := PriorityBackground
			if i%3 == 0 {
				priority = PriorityInteractive
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			permit, err := governor.Acquire(ctx, identity, "cdn.example", priority)
			if err != nil {
				t.Errorf("Acquire() error = %v", err)
				return
			}
			current := active.Add(1)
			for {
				observed := peak.Load()
				if current <= observed || peak.CompareAndSwap(observed, current) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
			permit.Release()
		}()
	}
	wg.Wait()
	if got := peak.Load(); got > 4 {
		t.Fatalf("peak concurrency = %d, want <= 4", got)
	}
	stats := governor.Snapshot()
	if stats.Active != 0 || stats.WaitingInteractive != 0 || stats.WaitingBackground != 0 {
		t.Fatalf("governor leaked admission state: %+v", stats)
	}
}

func TestRetryAfterSupportsSecondsAndHTTPDate(t *testing.T) {
	now := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	seconds := make(http.Header)
	seconds.Set("Retry-After", "12")
	if got := retryAfter(seconds, now); got != 12*time.Second {
		t.Fatalf("seconds Retry-After = %s", got)
	}

	date := make(http.Header)
	date.Set("Retry-After", now.Add(45*time.Second).Format(http.TimeFormat))
	if got := retryAfter(date, now); got != 45*time.Second {
		t.Fatalf("date Retry-After = %s", got)
	}
}

func TestGovernorBoundsIdleTrafficStates(t *testing.T) {
	governor := testGovernor(2)
	for i := range maxTrafficStates + 25 {
		governor.Observe(
			Identity{Provider: fmt.Sprintf("provider-%d", i)},
			"cdn.example",
			http.StatusOK,
			nil,
		)
	}
	governor.mu.Lock()
	states := len(governor.states)
	governor.mu.Unlock()
	if states > maxTrafficStates {
		t.Fatalf("traffic states = %d, want <= %d", states, maxTrafficStates)
	}
}
