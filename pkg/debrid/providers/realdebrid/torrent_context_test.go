package realdebrid

import (
	"context"
	"errors"
	"testing"
)

func TestGetTorrentsContextRejectsCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&RealDebrid{}).GetTorrentsContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetTorrentsContext() error = %v, want context.Canceled", err)
	}
}
