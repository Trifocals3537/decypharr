package webdav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager"
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
