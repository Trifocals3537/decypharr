package torbox

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestGetTorboxStatusClassifiesTransientStates(t *testing.T) {
	client := &Torbox{}
	tests := []struct {
		name     string
		state    string
		finished bool
		want     types.TorrentStatus
	}{
		{name: "account queue", state: "queued", want: types.TorrentStatusQueued},
		{name: "download queue", state: "queuedDL", want: types.TorrentStatusQueued},
		{name: "upload queue", state: "queuedUP", want: types.TorrentStatusQueued},
		{name: "queue details and case", state: " QUEUED (Waiting for slot) ", want: types.TorrentStatusQueued},
		{name: "stalled without seeds", state: "stalled (No seeds)", want: types.TorrentStatusDownloading},
		{name: "qbit stalled download", state: "stalledDL", want: types.TorrentStatusDownloading},
		{name: "metadata", state: "metaDL", want: types.TorrentStatusDownloading},
		{name: "text complete is not authoritative", state: "completed", want: types.TorrentStatusDownloading},
		{name: "authoritative completion", state: "queued", finished: true, want: types.TorrentStatusDownloaded},
		{name: "failed processing", state: "failed (processing)", want: types.TorrentStatusError},
		{name: "incomplete expired download", state: "incomplete", want: types.TorrentStatusError},
		{name: "unknown", state: "future-provider-state", want: types.TorrentStatusError},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := client.getTorboxStatus(test.state, test.finished); got != test.want {
				t.Fatalf("getTorboxStatus(%q, %t) = %q, want %q", test.state, test.finished, got, test.want)
			}
		})
	}
}

func TestCheckStatusKeepsQueuedTorrent(t *testing.T) {
	config.SetConfigPath(t.TempDir())
	for _, state := range []string{"queued", "queuedDL", "queuedUP"} {
		t.Run(state, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/torrents/mylist" {
					t.Errorf("path = %q, want /api/torrents/mylist", r.URL.Path)
				}
				if got := r.URL.Query().Get("id"); got != "42" {
					t.Errorf("id = %q, want 42", got)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": true,
					"data": map[string]any{
						"id":                42,
						"name":              "Queued release",
						"hash":              "0123456789abcdef0123456789abcdef01234567",
						"download_state":    state,
						"download_finished": false,
						"files":             []any{},
					},
				})
			}))
			defer server.Close()

			client := &Torbox{
				Host:   server.URL,
				client: request.New(request.WithMaxRetries(0), request.WithTimeout(2*time.Second)),
				logger: zerolog.Nop(),
				config: config.Debrid{Name: "torbox"},
			}
			torrent, err := client.CheckStatus(&types.Torrent{
				Id:               "42",
				Name:             "Queued release",
				DownloadUncached: false,
			})
			if err != nil {
				t.Fatalf("CheckStatus() error = %v", err)
			}
			if torrent.Status != types.TorrentStatusQueued {
				t.Fatalf("CheckStatus() status = %q, want %q", torrent.Status, types.TorrentStatusQueued)
			}
		})
	}
}
