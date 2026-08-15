package link

import (
	"context"
	"errors"
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
	cacheLinks    bool
	cached        map[string]types.DownloadLink
	fetches       int
	invalidations int
	accounts      *account.Manager
}

func (c *lifecycleTestClient) GetDownloadLink(_ string, file *types.File) (types.DownloadLink, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cacheLinks {
		if cached, ok := c.cached[file.Link]; ok {
			return cached, nil
		}
	}
	c.fetches++
	if len(c.links) == 0 {
		return types.DownloadLink{}, fmt.Errorf("no test links configured")
	}
	index := min(c.fetches-1, len(c.links)-1)
	link := c.links[index]
	if c.cacheLinks {
		if c.cached == nil {
			c.cached = make(map[string]types.DownloadLink)
		}
		c.cached[file.Link] = link
	}
	return link, nil
}

func (c *lifecycleTestClient) InvalidateCachedLink(link types.DownloadLink) error {
	c.mu.Lock()
	c.invalidations++
	delete(c.cached, link.Link)
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

func TestRejectedReplacementStartsRefreshCooldown(t *testing.T) {
	var heads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		heads.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	client := &lifecycleTestClient{cacheLinks: true, links: []types.DownloadLink{
		lifecycleDownloadLink(server.URL + "/old"),
		lifecycleDownloadLink(server.URL + "/new"),
		lifecycleDownloadLink(server.URL + "/unexpected"),
	}}
	service := newLifecycleService(client, server.Client(), 0)
	now := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	entry := lifecycleTestEntry()

	_, err := service.GetLink(context.Background(), entry, "video.mkv")
	if err == nil {
		t.Fatal("GetLink() succeeded, want rejected replacement error")
	}
	if linkErr := GetLinkError(err); linkErr == nil || !linkErr.ShouldRefetch() || linkErr.Code != "403" {
		t.Fatalf("error = %v, want original refetchable rejection", err)
	}

	for attempt := range 2 {
		_, err = service.GetLink(context.Background(), entry, "video.mkv")
		linkErr := GetLinkError(err)
		if linkErr == nil || !linkErr.ShouldBackoff() || linkErr.Code != "link_refresh_cooldown" {
			t.Fatalf("cooldown attempt %d error = %v, want throttled refresh cooldown", attempt+1, err)
		}
		if linkErr.RetryAfter != refreshBackoffBase {
			t.Fatalf("cooldown attempt %d RetryAfter = %s, want %s", attempt+1, linkErr.RetryAfter, refreshBackoffBase)
		}
	}
	fetches, invalidations := client.counts()
	if fetches != 2 || invalidations != 1 {
		t.Fatalf("provider fetches/invalidations = %d/%d, want bounded 2/1", fetches, invalidations)
	}
	if got := heads.Load(); got != 4 {
		t.Fatalf("HEAD requests = %d, want 4 so the cached CDN URL remains probeable", got)
	}
}

func TestRefreshCooldownClearsWhenCachedLinkRecovers(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusForbidden)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()

	client := &lifecycleTestClient{cacheLinks: true, links: []types.DownloadLink{
		lifecycleDownloadLink(server.URL + "/old"),
		lifecycleDownloadLink(server.URL + "/replacement"),
	}}
	service := newLifecycleService(client, server.Client(), 0)
	entry := lifecycleTestEntry()

	if _, err := service.GetLink(context.Background(), entry, "video.mkv"); err == nil {
		t.Fatal("first GetLink() succeeded, want rejected replacement")
	}
	status.Store(http.StatusOK)
	got, err := service.GetLink(context.Background(), entry, "video.mkv")
	if err != nil {
		t.Fatalf("recovery GetLink() error = %v", err)
	}
	if got.DownloadLink != server.URL+"/replacement" {
		t.Fatalf("recovered link = %q, want cached replacement", got.DownloadLink)
	}
	fetches, invalidations := client.counts()
	if fetches != 2 || invalidations != 1 {
		t.Fatalf("provider fetches/invalidations = %d/%d, want 2/1 without another regeneration", fetches, invalidations)
	}
	service.refreshMu.Lock()
	remaining := len(service.refreshBackoffs)
	service.refreshMu.Unlock()
	if remaining != 0 {
		t.Fatalf("refresh backoffs = %d, want recovery to clear state", remaining)
	}
}

func TestRefreshCooldownExpiresAndBacksOffExponentially(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	client := &lifecycleTestClient{cacheLinks: true, links: []types.DownloadLink{
		lifecycleDownloadLink(server.URL + "/old"),
		lifecycleDownloadLink(server.URL + "/replacement-1"),
		lifecycleDownloadLink(server.URL + "/replacement-2"),
	}}
	service := newLifecycleService(client, server.Client(), 0)
	now := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	entry := lifecycleTestEntry()

	if _, err := service.GetLink(context.Background(), entry, "video.mkv"); err == nil {
		t.Fatal("first GetLink() succeeded, want rejected replacement")
	}
	now = now.Add(refreshBackoffBase)
	if _, err := service.GetLink(context.Background(), entry, "video.mkv"); err == nil {
		t.Fatal("post-cooldown GetLink() succeeded, want second rejected replacement")
	}

	_, err := service.GetLink(context.Background(), entry, "video.mkv")
	linkErr := GetLinkError(err)
	if linkErr == nil || linkErr.Code != "link_refresh_cooldown" || linkErr.RetryAfter != 2*refreshBackoffBase {
		t.Fatalf("error = %v (RetryAfter %v), want second 1m cooldown", err, linkErrRetryAfter(linkErr))
	}
	fetches, invalidations := client.counts()
	if fetches != 3 || invalidations != 2 {
		t.Fatalf("provider fetches/invalidations = %d/%d, want one new refresh after expiry (3/2)", fetches, invalidations)
	}
}

func linkErrRetryAfter(err *Error) time.Duration {
	if err == nil {
		return 0
	}
	return err.RetryAfter
}

func TestGetLinkAndRefreshShareOneProviderRegeneration(t *testing.T) {
	replacementStarted := make(chan struct{})
	releaseReplacement := make(chan struct{})
	var startOnce sync.Once
	var heads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		heads.Add(1)
		switch r.URL.Path {
		case "/old":
			w.WriteHeader(http.StatusForbidden)
		case "/replacement":
			startOnce.Do(func() { close(replacementStarted) })
			<-releaseReplacement
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	old := lifecycleDownloadLink(server.URL + "/old")
	replacement := lifecycleDownloadLink(server.URL + "/replacement")
	unexpected := lifecycleDownloadLink(server.URL + "/unexpected")
	client := &lifecycleTestClient{cacheLinks: true, links: []types.DownloadLink{old, replacement, unexpected}}
	service := newLifecycleService(client, server.Client(), 0)
	entry := lifecycleTestEntry()

	type refreshResult struct {
		link types.DownloadLink
		err  error
	}
	firstResult := make(chan refreshResult, 1)
	go func() {
		link, err := service.GetLink(context.Background(), entry, "video.mkv")
		firstResult <- refreshResult{link: link, err: err}
	}()
	<-replacementStarted

	releaseTimer := time.AfterFunc(100*time.Millisecond, func() { close(releaseReplacement) })
	secondLink, secondErr := service.Refresh(context.Background(), entry, old)
	first := <-firstResult
	_ = releaseTimer.Stop()
	if first.err != nil || secondErr != nil {
		t.Fatalf("concurrent GetLink()/Refresh() errors = %v / %v", first.err, secondErr)
	}
	if first.link.DownloadLink != replacement.DownloadLink || secondLink.DownloadLink != replacement.DownloadLink {
		t.Fatalf("concurrent links = %q / %q, want shared replacement", first.link.DownloadLink, secondLink.DownloadLink)
	}
	fetches, invalidations := client.counts()
	if fetches != 2 || invalidations != 1 || heads.Load() != 2 {
		t.Fatalf("provider fetches/invalidations/HEADs = %d/%d/%d, want 2/1/2", fetches, invalidations, heads.Load())
	}
}

func TestCanceledRefreshDoesNotCreateBackoff(t *testing.T) {
	client := &lifecycleTestClient{}
	service := newLifecycleService(client, http.DefaultClient, 0)
	entry := lifecycleTestEntry()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := service.Refresh(ctx, entry, lifecycleDownloadLink("https://cdn.example/rejected"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Refresh() error = %v, want context cancellation", err)
	}
	fetches, invalidations := client.counts()
	if fetches != 0 || invalidations != 0 {
		t.Fatalf("provider fetches/invalidations = %d/%d, want 0/0", fetches, invalidations)
	}
	service.refreshMu.Lock()
	states := len(service.refreshBackoffs)
	service.refreshMu.Unlock()
	if states != 0 {
		t.Fatalf("refresh backoffs = %d, want none after cancellation", states)
	}
}

func TestRefreshBackoffIsScopedByProviderPlacementAndFile(t *testing.T) {
	service := newLifecycleService(&lifecycleTestClient{}, http.DefaultClient, 0)
	now := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	entry := lifecycleTestEntry()
	baseKey := linkLifecycleKey(entry, "video.mkv")
	fileKey := linkLifecycleKey(entry, "extras/trailer.mkv")
	otherPlacement := lifecycleTestEntry()
	otherPlacement.Providers["test"].ID = "replacement-torrent"
	placementKey := linkLifecycleKey(otherPlacement, "video.mkv")
	otherProvider := lifecycleTestEntry()
	otherProvider.ActiveProvider = "other"
	otherProvider.Providers["other"] = &storage.ProviderEntry{Provider: "other", ID: "torrent"}
	providerKey := linkLifecycleKey(otherProvider, "video.mkv")

	service.recordRefreshFailure(baseKey)
	if _, blocked := service.refreshDelay(baseKey); !blocked {
		t.Fatal("base file is not in cooldown")
	}
	for label, key := range map[string]string{
		"file":      fileKey,
		"placement": placementKey,
		"provider":  providerKey,
	} {
		if _, blocked := service.refreshDelay(key); blocked {
			t.Fatalf("%s key unexpectedly inherited another file's cooldown", label)
		}
	}
}

func TestRefreshBackoffStateIsBoundedAndClearable(t *testing.T) {
	service := newLifecycleService(&lifecycleTestClient{}, http.DefaultClient, 0)
	service.now = func() time.Time {
		return time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	}

	for index := range maxRefreshBackoffs + 17 {
		service.recordRefreshFailure(fmt.Sprintf("key-%d", index))
	}
	service.refreshMu.Lock()
	states := len(service.refreshBackoffs)
	service.refreshMu.Unlock()
	if states != maxRefreshBackoffs {
		t.Fatalf("refresh backoffs = %d, want bounded %d", states, maxRefreshBackoffs)
	}

	service.validated.Store("validated", struct{}{})
	service.Clear()
	service.refreshMu.Lock()
	states = len(service.refreshBackoffs)
	service.refreshMu.Unlock()
	if states != 0 || service.validated.Size() != 0 {
		t.Fatalf("state after Clear() = %d backoffs/%d validations, want 0/0", states, service.validated.Size())
	}
}

func TestRefreshBackoffDelayCapsAtMaximum(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 1, want: 30 * time.Second},
		{failures: 2, want: time.Minute},
		{failures: 3, want: 2 * time.Minute},
		{failures: 4, want: 4 * time.Minute},
		{failures: 5, want: 5 * time.Minute},
		{failures: 20, want: 5 * time.Minute},
	}
	for _, test := range tests {
		if got := refreshBackoffDelay(test.failures); got != test.want {
			t.Errorf("refreshBackoffDelay(%d) = %s, want %s", test.failures, got, test.want)
		}
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
