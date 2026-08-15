package arr

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
)

func TestMonitorCancellationReleasesConcurrentCallers(t *testing.T) {
	requestStarted := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(requestStarted) })
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	storage := &Storage{
		arrs:   xsync.NewMap[string, *Arr](),
		logger: zerolog.Nop(),
	}
	storage.AddOrUpdate(New("test-arr", server.URL, "test-token", false, nil, "", "config"))

	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan error, 2)
	go func() { results <- storage.Monitor(ctx) }()

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Arr monitor request did not start")
	}

	// A second scheduler invocation shares the in-flight cleanup. Both callers
	// must still release when the manager context is canceled.
	go func() { results <- storage.Monitor(ctx) }()
	cancel()

	for range 2 {
		select {
		case err := <-results:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("Monitor() error = %v, want nil or context cancellation", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Arr monitor did not stop after cancellation")
		}
	}
}
