package realdebrid

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestDoGetRejectsTrailingJSON(t *testing.T) {
	config.SetConfigPath(t.TempDir())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{} {}`))
	}))
	defer server.Close()

	client := &RealDebrid{Host: server.URL, client: request.New()}
	var result map[string]any
	if _, err := client.doGet("/response", &result); err == nil ||
		!strings.Contains(err.Error(), "multiple values") {
		t.Fatalf("doGet error = %v, want trailing JSON rejection", err)
	}
}

func TestGetTorrentsUsesConfiguredLimitAsPageSizeNotSnapshotLimit(t *testing.T) {
	config.SetConfigPath(t.TempDir())

	remote := []TorrentsResponse{
		{Id: "1", Filename: "one", Status: "downloaded"},
		{Id: "2", Filename: "two", Status: "downloaded"},
		{Id: "3", Filename: "three", Status: "downloaded"},
	}
	offsets := make([]int, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offsets = append(offsets, offset)
		w.Header().Set("X-Total-Count", strconv.Itoa(len(remote)))
		w.Header().Set("Content-Type", "application/json")
		end := min(offset+limit, len(remote))
		if offset >= len(remote) {
			_ = json.NewEncoder(w).Encode([]TorrentsResponse{})
			return
		}
		_ = json.NewEncoder(w).Encode(remote[offset:end])
	}))
	defer server.Close()

	client := &RealDebrid{
		Host:   server.URL,
		client: request.New(request.WithMaxRetries(0)),
		config: config.Debrid{Name: "rd", Limit: 1},
	}
	torrents, err := client.GetTorrents()
	if err != nil {
		t.Fatal(err)
	}
	if len(torrents) != 3 {
		t.Fatalf("torrent count = %d, want complete 3-item snapshot", len(torrents))
	}
	if fmt.Sprint(offsets) != "[0 1 2]" {
		t.Fatalf("requested offsets = %v, want [0 1 2]", offsets)
	}
}

func TestGetTorrentsAdvancesByRemotePageSizeAndDiscardsPartialErrors(t *testing.T) {
	config.SetConfigPath(t.TempDir())

	t.Run("non-downloaded page still advances offset", func(t *testing.T) {
		remote := []TorrentsResponse{
			{Id: "1", Filename: "active", Status: "downloading"},
			{Id: "2", Filename: "ready", Status: "downloaded"},
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			w.Header().Set("X-Total-Count", strconv.Itoa(len(remote)))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(remote[offset : offset+1])
		}))
		defer server.Close()

		client := &RealDebrid{
			Host:   server.URL,
			client: request.New(request.WithMaxRetries(0)),
			config: config.Debrid{Name: "rd", Limit: 1},
		}
		torrents, err := client.GetTorrents()
		if err != nil {
			t.Fatal(err)
		}
		if len(torrents) != 1 || torrents[0].Id != "2" {
			t.Fatalf("torrents = %#v, want downloaded second page", torrents)
		}
	})

	t.Run("later page error returns no partial snapshot", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("offset") == "0" {
				w.Header().Set("X-Total-Count", "2")
				_, _ = w.Write([]byte(`[{"id":"1","filename":"one","status":"downloaded"}]`))
				return
			}
			http.Error(w, "do-not-expose", http.StatusTeapot)
		}))
		defer server.Close()

		client := &RealDebrid{
			Host:   server.URL,
			client: request.New(request.WithMaxRetries(0)),
			config: config.Debrid{Name: "rd", Limit: 1},
		}
		torrents, err := client.GetTorrents()
		if err == nil || len(torrents) != 0 || strings.Contains(err.Error(), "do-not-expose") {
			t.Fatalf("torrents = %#v, error = %v", torrents, err)
		}
	})
}

func TestGetTorrentFilesPreservesNestedDuplicateBasenames(t *testing.T) {
	var data torrentInfo
	data.Files = append(
		data.Files,
		struct {
			ID       int    `json:"id"`
			Path     string `json:"path"`
			Bytes    int64  `json:"bytes"`
			Selected int    `json:"selected"`
		}{ID: 1, Path: "/Release/Season 01/Episode.mkv", Bytes: 1024},
		struct {
			ID       int    `json:"id"`
			Path     string `json:"path"`
			Bytes    int64  `json:"bytes"`
			Selected int    `json:"selected"`
		}{ID: 2, Path: "/Release/Season 02/Episode.mkv", Bytes: 2048},
	)

	files, err := (&RealDebrid{}).getTorrentFiles(&types.Torrent{Id: "rd"}, data)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"Release/Season 01/Episode.mkv",
		"Release/Season 02/Episode.mkv",
	} {
		if file, exists := files[name]; !exists || file.Name != name || file.Path != name {
			t.Fatalf("file %q = %#v, exists=%v", name, file, exists)
		}
	}
}
