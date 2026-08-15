package torbox

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

const cachedCheckTestHash = "0123456789abcdef0123456789abcdef01234567"

func newCachedCheckTorbox(t *testing.T, handler http.HandlerFunc) *Torbox {
	t.Helper()
	config.SetConfigPath(t.TempDir())
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Torbox{
		Host:   server.URL,
		client: request.New(request.WithMaxRetries(0)),
		config: config.Debrid{Name: "torbox"},
	}
}

func TestIsCachedSeparatesUnknownFromDefiniteMiss(t *testing.T) {
	tests := []struct {
		name        string
		hash        string
		response    string
		status      int
		wantRequest bool
		wantCached  bool
		wantKnown   bool
	}{
		{
			name:        "cached",
			hash:        cachedCheckTestHash,
			response:    `{"success":true,"data":{"0123456789ABCDEF0123456789ABCDEF01234567":{"size":123}}}`,
			wantRequest: true,
			wantCached:  true,
			wantKnown:   true,
		},
		{
			name:        "definite miss",
			hash:        cachedCheckTestHash,
			response:    `{"success":true,"data":{}}`,
			wantRequest: true,
			wantKnown:   true,
		},
		{
			name:        "mismatched non-empty object",
			hash:        cachedCheckTestHash,
			response:    `{"success":true,"data":{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":{"size":123}}}`,
			wantRequest: true,
		},
		{
			name:        "matching incomplete object",
			hash:        cachedCheckTestHash,
			response:    `{"success":true,"data":{"` + cachedCheckTestHash + `":{"size":0}}}`,
			wantRequest: true,
		},
		{
			name:        "legacy null-data miss",
			hash:        cachedCheckTestHash,
			response:    `{"success":true,"detail":"No cached data found.","data":null}`,
			wantRequest: true,
			wantKnown:   true,
		},
		{
			name:        "ambiguous null data",
			hash:        cachedCheckTestHash,
			response:    `{"success":true,"data":null}`,
			wantRequest: true,
		},
		{
			name:        "unsuccessful envelope",
			hash:        cachedCheckTestHash,
			response:    `{"success":false,"data":{}}`,
			wantRequest: true,
		},
		{
			name:        "malformed response",
			hash:        cachedCheckTestHash,
			response:    `{`,
			wantRequest: true,
		},
		{
			name:        "non success status",
			hash:        cachedCheckTestHash,
			status:      http.StatusServiceUnavailable,
			wantRequest: true,
		},
		{
			name: "empty hash",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requested := false
			client := newCachedCheckTorbox(t, func(w http.ResponseWriter, r *http.Request) {
				requested = true
				if got := r.URL.Query().Get("hash"); got != test.hash {
					t.Errorf("hash query = %q, want %q", got, test.hash)
				}
				if got := r.URL.Query().Get("format"); got != "object" {
					t.Errorf("format query = %q, want object", got)
				}
				if got := r.URL.Query().Get("list_files"); got != "false" {
					t.Errorf("list_files query = %q, want false", got)
				}
				if test.status != 0 {
					w.WriteHeader(test.status)
					return
				}
				_, _ = w.Write([]byte(test.response))
			})

			cached, known := client.isCached(test.hash)
			if requested != test.wantRequest {
				t.Fatalf("request made = %t, want %t", requested, test.wantRequest)
			}
			if cached != test.wantCached || known != test.wantKnown {
				t.Fatalf(
					"isCached() = (%t, %t), want (%t, %t)",
					cached,
					known,
					test.wantCached,
					test.wantKnown,
				)
			}
		})
	}
}

func TestSubmitMagnetSkipsCreateOnlyForDefiniteCacheMiss(t *testing.T) {
	tests := []struct {
		name             string
		cacheResponse    string
		cacheStatus      int
		downloadUncached bool
		wantCacheCall    bool
		wantCreateCall   bool
		wantCacheMiss    bool
	}{
		{
			name:           "definite miss",
			cacheResponse:  `{"success":true,"data":{}}`,
			wantCacheCall:  true,
			wantCreateCall: false,
			wantCacheMiss:  true,
		},
		{
			name:           "cached",
			cacheResponse:  `{"success":true,"data":{"` + cachedCheckTestHash + `":{"size":123}}}`,
			wantCacheCall:  true,
			wantCreateCall: true,
		},
		{
			name:           "unknown probe",
			cacheStatus:    http.StatusServiceUnavailable,
			wantCacheCall:  true,
			wantCreateCall: true,
		},
		{
			name:             "uncached downloads enabled",
			downloadUncached: true,
			wantCreateCall:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cacheCalled := false
			createCalled := false
			client := newCachedCheckTorbox(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/torrents/checkcached":
					cacheCalled = true
					if test.cacheStatus != 0 {
						w.WriteHeader(test.cacheStatus)
						return
					}
					_, _ = w.Write([]byte(test.cacheResponse))
				case "/api/torrents/createtorrent":
					createCalled = true
					_, _ = w.Write([]byte(`{"success":true,"data":{"torrent_id":1,"hash":"` + cachedCheckTestHash + `"}}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})

			_, err := client.SubmitMagnet(&types.Torrent{
				InfoHash:         cachedCheckTestHash,
				Name:             "Release",
				DownloadUncached: test.downloadUncached,
				Magnet: &utils.Magnet{
					InfoHash: cachedCheckTestHash,
					Link:     "magnet:?xt=urn:btih:" + cachedCheckTestHash,
				},
			})

			if cacheCalled != test.wantCacheCall {
				t.Errorf("cache call = %t, want %t", cacheCalled, test.wantCacheCall)
			}
			if createCalled != test.wantCreateCall {
				t.Errorf("create call = %t, want %t", createCalled, test.wantCreateCall)
			}
			var customErr *customerror.Error
			isCacheMiss := errors.As(err, &customErr) && customErr.Code == "torrent_not_cached"
			if isCacheMiss != test.wantCacheMiss {
				t.Fatalf("error = %v, cache miss = %t, want %t", err, isCacheMiss, test.wantCacheMiss)
			}
			if !test.wantCacheMiss && err != nil {
				t.Fatalf("SubmitMagnet() error = %v", err)
			}
		})
	}
}
