package premiumize

import (
	"context"
	"errors"
	"testing"
)

func TestGetTorrentsContextRejectsCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Premiumize{}).GetTorrentsContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetTorrentsContext() error = %v, want context.Canceled", err)
	}
}

func TestMaintenanceContextRejectsCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider := &Premiumize{}
	if err := provider.RefreshDownloadLinksContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RefreshDownloadLinksContext() error = %v, want context.Canceled", err)
	}
	if err := provider.SyncAccountsContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("SyncAccountsContext() error = %v, want context.Canceled", err)
	}
}
