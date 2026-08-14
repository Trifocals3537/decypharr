package link

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type lifecycleTestClient struct {
	debrid.Client
	mu            sync.Mutex
	links         []types.DownloadLink
	fetches       int
	invalidations int
	accounts      *account.Manager
}

func (c *lifecycleTestClient) GetDownloadLink(_ string, _ *types.File) (types.DownloadLink, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetches++
	if len(c.links) == 0 {
		return types.DownloadLink{}, fmt.Errorf("no test links configured")
	}
	index := min(c.fetches-1, len(c.links)-1)
	return c.links[index], nil
}

func (c *lifecycleTestClient) InvalidateCachedLink(types.DownloadLink) error {
	c.mu.Lock()
	c.invalidations++
	c.mu.Unlock()
	return nil
}

func (c *lifecycleTestClient) AccountManager() *account.Manager {
	return c.accounts
}

func (c *lifecycleTestClient) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetches, c.invalidations
}

func lifecycleTestEntry() *storage.Entry {
	const filename = "video.mkv"
	return &storage.Entry{
		InfoHash:       "hash",
		Name:           "entry",
		ActiveProvider: "test",
		Files: map[string]*storage.File{
			filename: {Name: filename, Size: 4},
		},
		Providers: map[string]*storage.ProviderEntry{
			"test": {
				Provider: "test",
				ID:       "torrent",
				Files: map[string]*storage.ProviderFile{
					filename: {Link: "restricted-link"},
				},
			},
		},
	}
}

func lifecycleDownloadLink(url string) types.DownloadLink {
	return types.DownloadLink{
		Debrid:       "test",
		Token:        "token",
		Filename:     "video.mkv",
		Link:         "restricted-link",
		DownloadLink: url,
		Generated:    time.Now(),
		ExpiresAt:    time.Now().Add(time.Hour),
	}
}

func newLifecycleService(client *lifecycleTestClient, httpClient *http.Client, retries int) *Service {
	clients := xsync.NewMap[string, debrid.Client]()
	clients.Store("test", client)
	return New(clients, nil, nil, nil, httpClient, retries, zerolog.Nop())
}

func TestTransientValidationFailureIsNotSticky(t *testing.T) {
	var heads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if heads.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &lifecycleTestClient{links: []types.DownloadLink{lifecycleDownloadLink(server.URL)}}
	service := newLifecycleService(client, server.Client(), 0)
	entry := lifecycleTestEntry()

	if _, err := service.GetLink(context.Background(), entry, "video.mkv"); err == nil {
		t.Fatal("first GetLink() succeeded, want transient failure")
	}
	if _, err := service.GetLink(context.Background(), entry, "video.mkv"); err != nil {
		t.Fatalf("second GetLink() error = %v", err)
	}
	if _, err := service.GetLink(context.Background(), entry, "video.mkv"); err != nil {
		t.Fatalf("memoized GetLink() error = %v", err)
	}
	if got := heads.Load(); got != 2 {
		t.Fatalf("HEAD requests = %d, want 2", got)
	}
}

func TestThrottleBacksOffWithoutGeneratingAnotherLink(t *testing.T) {
	var heads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if heads.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &lifecycleTestClient{links: []types.DownloadLink{lifecycleDownloadLink(server.URL)}}
	service := newLifecycleService(client, server.Client(), 1)
	var waits []time.Duration
	service.wait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}

	if _, err := service.GetLink(context.Background(), lifecycleTestEntry(), "video.mkv"); err != nil {
		t.Fatalf("GetLink() error = %v", err)
	}
	fetches, invalidations := client.counts()
	if fetches != 1 || invalidations != 0 {
		t.Fatalf("fetches/invalidations = %d/%d, want 1/0", fetches, invalidations)
	}
	if len(waits) != 1 || waits[0] != 3*time.Second {
		t.Fatalf("waits = %v, want [3s]", waits)
	}
}

func TestRejectedLinkRefreshesOnceAndValidatesReplacement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &lifecycleTestClient{links: []types.DownloadLink{
		lifecycleDownloadLink(server.URL + "/old"),
		lifecycleDownloadLink(server.URL + "/new"),
	}}
	service := newLifecycleService(client, server.Client(), 0)
	got, err := service.GetLink(context.Background(), lifecycleTestEntry(), "video.mkv")
	if err != nil {
		t.Fatalf("GetLink() error = %v", err)
	}
	if got.DownloadLink != server.URL+"/new" {
		t.Fatalf("download link = %q, want replacement", got.DownloadLink)
	}
	fetches, invalidations := client.counts()
	if fetches != 2 || invalidations != 1 {
		t.Fatalf("fetches/invalidations = %d/%d, want 2/1", fetches, invalidations)
	}
}

func TestRejectedReplacementDoesNotLoop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	client := &lifecycleTestClient{links: []types.DownloadLink{
		lifecycleDownloadLink(server.URL + "/old"),
		lifecycleDownloadLink(server.URL + "/new"),
		lifecycleDownloadLink(server.URL + "/unexpected"),
	}}
	service := newLifecycleService(client, server.Client(), 0)
	_, err := service.GetLink(context.Background(), lifecycleTestEntry(), "video.mkv")
	if err == nil {
		t.Fatal("GetLink() succeeded, want rejected replacement error")
	}
	if linkErr := GetLinkError(err); linkErr == nil || !linkErr.IsPermanent() || linkErr.Code != "link_refresh_exhausted" {
		t.Fatalf("error = %v, want permanent exhausted-refresh error", err)
	}
	fetches, invalidations := client.counts()
	if fetches != 2 || invalidations != 1 {
		t.Fatalf("fetches/invalidations = %d/%d, want bounded 2/1", fetches, invalidations)
	}
}

func TestValidationFallsBackToOneByteGetWhenHeadUnsupported(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+":"+r.Header.Get("Range"))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusPartialContent)
	}))
	defer server.Close()

	client := &lifecycleTestClient{links: []types.DownloadLink{lifecycleDownloadLink(server.URL)}}
	service := newLifecycleService(client, server.Client(), 0)
	if _, err := service.GetLink(context.Background(), lifecycleTestEntry(), "video.mkv"); err != nil {
		t.Fatalf("GetLink() error = %v", err)
	}
	want := []string{"HEAD:", "GET:bytes=0-0"}
	if fmt.Sprint(methods) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", methods, want)
	}
}

func TestRegeneratedLinkWithSameURLIsRevalidated(t *testing.T) {
	var heads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		heads.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	first := lifecycleDownloadLink(server.URL)
	second := first
	second.Generated = first.Generated.Add(time.Minute)
	second.ExpiresAt = first.ExpiresAt.Add(time.Minute)
	client := &lifecycleTestClient{links: []types.DownloadLink{first, second}}
	service := newLifecycleService(client, server.Client(), 0)
	entry := lifecycleTestEntry()

	if _, err := service.GetLink(context.Background(), entry, "video.mkv"); err != nil {
		t.Fatalf("first GetLink() error = %v", err)
	}
	if _, err := service.GetLink(context.Background(), entry, "video.mkv"); err != nil {
		t.Fatalf("second GetLink() error = %v", err)
	}
	if got := heads.Load(); got != 2 {
		t.Fatalf("HEAD requests = %d, want 2 for two link generations", got)
	}
}

func TestAccountFailureStopsWhenNoAlternateAccountExists(t *testing.T) {
	previousPath := config.GetMainPath()
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(func() {
		config.Reset()
		config.SetConfigPath(previousPath)
	})

	var heads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		heads.Add(1)
		w.Header().Set("X-Error", "bandwidth_exceeded")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	accounts := account.NewManager(config.Debrid{
		Name:            "test",
		DownloadAPIKeys: []string{"token"},
	}, nil, zerolog.Nop())
	client := &lifecycleTestClient{
		links:    []types.DownloadLink{lifecycleDownloadLink(server.URL)},
		accounts: accounts,
	}
	service := newLifecycleService(client, server.Client(), 0)
	_, err := service.GetLink(context.Background(), lifecycleTestEntry(), "video.mkv")
	if err == nil {
		t.Fatal("GetLink() succeeded, want no-active-account error")
	}
	if linkErr := GetLinkError(err); linkErr == nil || linkErr.Code != "no_active_account" || !linkErr.IsPermanent() {
		t.Fatalf("error = %v, want permanent no-active-account error", err)
	}
	if heads.Load() != 1 {
		t.Fatalf("HEAD requests = %d, want 1 without recursive fallback", heads.Load())
	}
}

func TestAccountFailureMovesToAlternateAccount(t *testing.T) {
	previousPath := config.GetMainPath()
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(func() {
		config.Reset()
		config.SetConfigPath(previousPath)
	})

	var heads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		heads.Add(1)
		if r.URL.Path == "/first" {
			w.Header().Set("X-Error", "bandwidth_exceeded")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	accounts := account.NewManager(config.Debrid{
		Name:            "test",
		DownloadAPIKeys: []string{"first-token", "second-token"},
	}, nil, zerolog.Nop())
	first := lifecycleDownloadLink(server.URL + "/first")
	first.Token = "first-token"
	second := lifecycleDownloadLink(server.URL + "/second")
	second.Token = "second-token"
	client := &lifecycleTestClient{
		links:    []types.DownloadLink{first, second},
		accounts: accounts,
	}
	service := newLifecycleService(client, server.Client(), 0)
	got, err := service.GetLink(context.Background(), lifecycleTestEntry(), "video.mkv")
	if err != nil {
		t.Fatalf("GetLink() error = %v", err)
	}
	if got.Token != "second-token" {
		t.Fatalf("link token = %q, want alternate account", got.Token)
	}
	if heads.Load() != 2 || len(accounts.Active()) != 1 || accounts.Active()[0].Token != "second-token" {
		t.Fatalf("HEAD requests/active accounts = %d/%v, want 2/[second-token]", heads.Load(), accounts.Active())
	}
}
