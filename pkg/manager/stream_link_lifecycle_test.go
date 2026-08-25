package manager

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type streamLifecycleLinkService struct {
	initial   debridTypes.DownloadLink
	refreshed debridTypes.DownloadLink
	gets      atomic.Int32
	refreshes atomic.Int32
}

func (s *streamLifecycleLinkService) GetLink(context.Context, *storage.Entry, string) (debridTypes.DownloadLink, error) {
	s.gets.Add(1)
	return s.initial, nil
}

func (s *streamLifecycleLinkService) Refresh(context.Context, *storage.Entry, debridTypes.DownloadLink) (debridTypes.DownloadLink, error) {
	s.refreshes.Add(1)
	return s.refreshed, nil
}

func (s *streamLifecycleLinkService) Clear() {}

func streamLifecycleEntry() *storage.Entry {
	return &storage.Entry{
		Files: map[string]*storage.File{
			"video.mkv": {Name: "video.mkv", Size: 4},
		},
	}
}

func TestStreamRefreshesRejectedLinkBeforeWritingBytes(t *testing.T) {
	var oldRequests atomic.Int32
	var newRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			oldRequests.Add(1)
			w.WriteHeader(http.StatusForbidden)
		case "/new":
			newRequests.Add(1)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("data"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	links := &streamLifecycleLinkService{
		initial:   debridTypes.DownloadLink{Filename: "video.mkv", DownloadLink: server.URL + "/old"},
		refreshed: debridTypes.DownloadLink{Filename: "video.mkv", DownloadLink: server.URL + "/new"},
	}
	manager := &Manager{
		linkService:  links,
		streamClient: server.Client(),
		config:       &config.Config{Retries: 0},
	}

	var output bytes.Buffer
	readyCalls := 0
	err := manager.Stream(context.Background(), streamLifecycleEntry(), "video.mkv", 0, 3, &output, func(*StreamMetadata) error {
		readyCalls++
		return nil
	}, "test")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if output.String() != "data" {
		t.Fatalf("streamed data = %q, want data", output.String())
	}
	if readyCalls != 1 || links.refreshes.Load() != 1 || oldRequests.Load() != 1 || newRequests.Load() != 1 {
		t.Fatalf("ready/refresh/old/new = %d/%d/%d/%d, want 1/1/1/1", readyCalls, links.refreshes.Load(), oldRequests.Load(), newRequests.Load())
	}
}

func TestStreamThrottleRetriesSameLink(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("data"))
	}))
	defer server.Close()

	links := &streamLifecycleLinkService{
		initial: debridTypes.DownloadLink{Filename: "video.mkv", DownloadLink: server.URL},
	}
	var waits []time.Duration
	manager := &Manager{
		linkService:  links,
		streamClient: server.Client(),
		config:       &config.Config{Retries: 1},
		streamWait: func(_ context.Context, delay time.Duration) error {
			waits = append(waits, delay)
			return nil
		},
	}

	var output bytes.Buffer
	if err := manager.Stream(context.Background(), streamLifecycleEntry(), "video.mkv", 0, 3, &output, nil, "test"); err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if requests.Load() != 2 || links.refreshes.Load() != 0 {
		t.Fatalf("requests/refreshes = %d/%d, want 2/0", requests.Load(), links.refreshes.Load())
	}
	if len(waits) != 1 || waits[0] != 4*time.Second {
		t.Fatalf("waits = %v, want [4s]", waits)
	}
}

func TestStreamDoesNotLoopWhenReplacementIsRejected(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	links := &streamLifecycleLinkService{
		initial:   debridTypes.DownloadLink{Filename: "video.mkv", DownloadLink: server.URL + "/old"},
		refreshed: debridTypes.DownloadLink{Filename: "video.mkv", DownloadLink: server.URL + "/new"},
	}
	manager := &Manager{
		linkService:  links,
		streamClient: server.Client(),
		config:       &config.Config{Retries: 3},
	}

	err := manager.Stream(context.Background(), streamLifecycleEntry(), "video.mkv", 0, 3, io.Discard, nil, "test")
	if err == nil {
		t.Fatal("Stream() succeeded, want rejected replacement error")
	}
	streamErr, ok := err.(StreamError)
	if !ok || streamErr.Retryable || !streamErr.LinkError {
		t.Fatalf("error = %#v, want non-retryable exhausted link refresh", err)
	}
	if requests.Load() != 2 || links.refreshes.Load() != 1 {
		t.Fatalf("requests/refreshes = %d/%d, want bounded 2/1", requests.Load(), links.refreshes.Load())
	}
}

type failingBody struct {
	offset int
}

func (b *failingBody) Read(p []byte) (int, error) {
	const data = "ab"
	if b.offset < len(data) {
		n := copy(p, data[b.offset:])
		b.offset += n
		return n, nil
	}
	return 0, io.ErrUnexpectedEOF
}

func (*failingBody) Close() error { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestStreamDoesNotRefreshOrReplayAfterBytesAreWritten(t *testing.T) {
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			Header:        make(http.Header),
			Body:          &failingBody{},
			ContentLength: 4,
		}, nil
	})}
	links := &streamLifecycleLinkService{
		initial: debridTypes.DownloadLink{Filename: "video.mkv", DownloadLink: "https://cdn.example/file"},
	}
	manager := &Manager{
		linkService:  links,
		streamClient: client,
		config:       &config.Config{Retries: 3},
	}

	var output bytes.Buffer
	readyCalls := 0
	err := manager.Stream(context.Background(), streamLifecycleEntry(), "video.mkv", 0, 3, &output, func(*StreamMetadata) error {
		readyCalls++
		return nil
	}, "test")
	if err == nil {
		t.Fatal("Stream() succeeded, want mid-body failure")
	}
	if output.String() != "ab" {
		t.Fatalf("streamed data = %q, want only first two bytes", output.String())
	}
	if requests.Load() != 1 || links.refreshes.Load() != 0 || readyCalls != 1 {
		t.Fatalf("requests/refreshes/ready = %d/%d/%d, want 1/0/1", requests.Load(), links.refreshes.Load(), readyCalls)
	}
}
