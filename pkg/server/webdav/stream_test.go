package webdav

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/link"
	"github.com/sirrobot01/decypharr/pkg/storage"
	strmurl "github.com/sirrobot01/decypharr/pkg/strm"
)

const webdavTestStrmSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func identityRequest(method, target, infohash, fileID string) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("infohash", infohash)
	routeContext.URLParams.Add("fileID", fileID)
	routeContext.URLParams.Add("name", "Movie.mkv")
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)
	return request.WithContext(ctx)
}

func TestGetRangeReturnsLogicalClientRanges(t *testing.T) {
	tests := []struct {
		name              string
		header            string
		wantStart         int64
		wantEnd           int64
		wantErrorContains string
	}{
		{name: "full file", wantStart: 0, wantEnd: -1},
		{name: "single range", header: "bytes=1-2", wantStart: 1, wantEnd: 2},
		{name: "suffix range", header: "bytes=-2", wantStart: 2, wantEnd: 3},
		{name: "unsatisfiable", header: "bytes=9-10", wantErrorContains: "not satisfiable"},
		{name: "malformed", header: "bytes=invalid", wantErrorContains: "invalid range"},
		{name: "multiple ranges", header: "bytes=0-0,2-2", wantErrorContains: "multiple ranges"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/movie.mkv", nil)
			if test.header != "" {
				request.Header.Set("Range", test.header)
			}
			start, end, err := getRange(4, request)
			if test.wantErrorContains != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrorContains) {
					t.Fatalf("getRange() = %d-%d, %v; want error containing %q", start, end, err, test.wantErrorContains)
				}
				return
			}
			if err != nil || start != test.wantStart || end != test.wantEnd {
				t.Fatalf("getRange() = %d-%d, %v; want %d-%d", start, end, err, test.wantStart, test.wantEnd)
			}
		})
	}
}

func TestNormalizeStreamErrorUsesRetryableProviderSemantics(t *testing.T) {
	providerErr := manager.StreamError{
		Err:       link.NewRetryableError(errors.New("signed-url-secret"), "503"),
		Retryable: true,
	}
	streamErr := normalizeStreamError(providerErr, false)
	if streamErr.HTTPStatus() != http.StatusServiceUnavailable || !streamErr.IsRetryable() ||
		streamErr.Code != "stream.provider_unavailable" {
		t.Fatalf("normalized error = status %d retryable %v code %q",
			streamErr.HTTPStatus(), streamErr.IsRetryable(), streamErr.Code)
	}

	handler := &Handler{}
	response := httptest.NewRecorder()
	handler.writeStreamError("movie", streamErr, response)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("response = status %d Retry-After %q", response.Code, response.Header().Get("Retry-After"))
	}
	if body := response.Body.String(); body != "Service Unavailable\n" || strings.Contains(body, "signed-url-secret") {
		t.Fatalf("unsafe response body = %q", body)
	}
}

func TestWriteStreamErrorPreservesProviderRetryAfter(t *testing.T) {
	linkErr := link.NewLinkError(errors.New("provider cooldown"), link.CategoryThrottled, "account_cooldown")
	linkErr.RetryAfter = 90*time.Second + time.Millisecond
	streamErr := normalizeStreamError(manager.StreamError{Err: linkErr, Retryable: true}, false)

	handler := &Handler{}
	response := httptest.NewRecorder()
	handler.writeStreamError("movie", streamErr, response)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "91" {
		t.Fatalf("response = status %d Retry-After %q", response.Code, response.Header().Get("Retry-After"))
	}
}

func TestNormalizeStreamErrorPreservesCustomStatusAndSilencesCancellation(t *testing.T) {
	existing := customerror.NewArticleNotFoundError(errors.New("article missing"))
	if got := normalizeStreamError(existing, false); got != existing || got.HTTPStatus() != http.StatusGone {
		t.Fatalf("custom error was not preserved: %+v", got)
	}

	canceled := normalizeStreamError(context.Canceled, false)
	response := httptest.NewRecorder()
	(&Handler{}).writeStreamError("movie", canceled, response)
	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("cancellation wrote response status/body = %d/%q", response.Code, response.Body.String())
	}
}

func TestIdentityStreamAlwaysRequiresSignature(t *testing.T) {
	configRoot := t.TempDir()
	config.SetConfigPath(configRoot)
	config.Reset()
	t.Cleanup(config.Reset)
	cfg := config.Get()
	cfg.Strm = config.Strm{
		Enabled: true, Path: filepath.Join(configRoot, "strm"), Secret: webdavTestStrmSecret,
	}

	handler := &Handler{}
	response := httptest.NewRecorder()
	handler.handleIdentityStream(response, identityRequest(http.MethodGet, "/stream/v1/entry/file/Movie.mkv", "entry", "file"))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
}

func TestIdentityStreamHEADUsesStoredMetadata(t *testing.T) {
	configRoot := t.TempDir()
	config.SetConfigPath(configRoot)
	config.Reset()
	t.Cleanup(config.Reset)
	cfg := config.Get()
	cfg.Strm = config.Strm{}

	mgr := manager.New()
	// The process-wide logger intentionally keeps its file open. Register its
	// close before manager shutdown so Windows can remove the temporary config
	// directory after every manager-owned final log line has been written.
	t.Cleanup(func() {
		if err := logger.Close(); err != nil {
			t.Errorf("close logger: %v", err)
		}
	})
	t.Cleanup(func() {
		if err := mgr.Stop(); err != nil {
			t.Errorf("stop manager: %v", err)
		}
	})
	entry := &storage.Entry{
		InfoHash: "entry", Name: "Movie", IsComplete: true,
		Files: map[string]*storage.File{
			"Movie.mkv": {Name: "Movie.mkv", Size: 12345, AddedOn: time.Unix(100, 0)},
		},
	}
	if err := mgr.AddOrUpdate(entry, nil); err != nil {
		t.Fatal(err)
	}
	fileID := entry.Files["Movie.mkv"].ID
	cfg.Strm = config.Strm{
		Enabled: true, Path: filepath.Join(configRoot, "strm"), Secret: webdavTestStrmSecret,
	}
	signature, err := strmurl.Sign(webdavTestStrmSecret, "stream", entry.InfoHash, fileID)
	if err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(mgr)
	response := httptest.NewRecorder()
	target := "/stream/v1/entry/" + fileID + "/Movie.mkv?s=" + signature
	handler.handleIdentityStream(response, identityRequest(http.MethodHead, target, entry.InfoHash, fileID))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Length"); got != "12345" {
		t.Fatalf("Content-Length = %q", got)
	}
	if mgr.GetActiveStreamsCount() != 0 {
		t.Fatal("HEAD request registered an active stream")
	}
}
