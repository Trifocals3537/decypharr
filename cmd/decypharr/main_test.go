package decypharr

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type serviceFunc func(context.Context) error

func (f serviceFunc) Start(ctx context.Context) error {
	return f(ctx)
}

func TestStartServicesPropagatesErrorAndCancelsPeer(t *testing.T) {
	expected := errors.New("address already in use")
	peerCanceled := make(chan struct{})

	managerService := serviceFunc(func(ctx context.Context) error {
		<-ctx.Done()
		close(peerCanceled)
		return nil
	})
	httpService := serviceFunc(func(context.Context) error {
		return expected
	})

	err := startServices(context.Background(), managerService, httpService)
	if !errors.Is(err, expected) {
		t.Fatalf("startServices() error = %v, want %v", err, expected)
	}

	select {
	case <-peerCanceled:
	default:
		t.Fatal("peer service was not canceled after service failure")
	}
}

func TestStartServicesWaitsForServiceShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)

	managerService := serviceFunc(func(context.Context) error {
		return nil
	})
	httpService := serviceFunc(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		<-release
		return nil
	})

	go func() {
		result <- startServices(ctx, managerService, httpService)
	}()

	<-started
	cancel()

	select {
	case err := <-result:
		t.Fatalf("startServices() returned before service cleanup completed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("startServices() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("startServices() did not return after service cleanup completed")
	}
}

func TestStartServicesConvertsPanicToError(t *testing.T) {
	managerService := serviceFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})
	httpService := serviceFunc(func(context.Context) error {
		panic("boom")
	})

	err := startServices(context.Background(), managerService, httpService)
	if err == nil {
		t.Fatal("startServices() error = nil, want panic error")
	}
	if !strings.Contains(err.Error(), "HTTP service panicked: boom") {
		t.Fatalf("startServices() error = %q, want named panic", err)
	}
}

func TestStartServicesRejectsUnexpectedCleanExit(t *testing.T) {
	service := serviceFunc(func(context.Context) error {
		return nil
	})

	err := startServices(context.Background(), service, service)
	if !errors.Is(err, errServicesStopped) {
		t.Fatalf("startServices() error = %v, want %v", err, errServicesStopped)
	}
}
