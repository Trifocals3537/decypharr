package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestRefreshLinksContextCancelsInFlightFetcher(t *testing.T) {
	t.Parallel()

	acc := newLinkCacheTestAccount()
	manager := contextTestManager(acc)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- manager.RefreshLinksContext(ctx, func(ctx context.Context, _ *Account) ([]types.DownloadLink, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}()
	waitForContextSignal(t, started)
	cancel()
	if err := waitForContextResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("RefreshLinksContext() error = %v, want context.Canceled", err)
	}
}

func TestSyncContextCancelsInFlightSync(t *testing.T) {
	t.Parallel()

	manager := contextTestManager(newLinkCacheTestAccount())
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- manager.SyncContext(ctx, func(ctx context.Context, _ *Account) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	waitForContextSignal(t, started)
	cancel()
	if err := waitForContextResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("SyncContext() error = %v, want context.Canceled", err)
	}
}

func contextTestManager(accounts ...*Account) *Manager {
	manager := &Manager{
		debrid:   "test",
		accounts: xsync.NewMap[string, *Account](),
		logger:   zerolog.Nop(),
	}
	for _, acc := range accounts {
		manager.accounts.Store(acc.Token, acc)
	}
	return manager
}

func waitForContextSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for context-aware callback")
	}
}

func waitForContextResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for context-aware operation")
		return nil
	}
}
