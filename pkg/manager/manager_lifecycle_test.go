package manager

import (
	"context"
	"testing"
	"time"
)

func TestManagerStopCancelsAndWaitsForBackgroundTasks(t *testing.T) {
	m := &Manager{}
	m.resetLifecycle()

	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	m.startBackground("test task", func() {
		close(started)
		<-m.ctx.Done()
		close(canceled)
		<-release
	})

	<-started
	result := make(chan error, 1)
	go func() {
		result <- m.Stop()
	}()

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("manager did not cancel its background context")
	}

	select {
	case err := <-result:
		t.Fatalf("Manager.Stop() returned before background cleanup completed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Manager.Stop() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Manager.Stop() did not return after background cleanup completed")
	}
}

func TestRestoreActiveDownloadJobsHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A pre-canceled restoration must return before touching an uninitialized
	// queue or any persisted entry state.
	(&Manager{}).restoreActiveDownloadJobs(ctx)
}
