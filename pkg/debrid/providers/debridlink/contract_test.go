package debridlink

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestGetTorrentUsesDocumentedListEndpointAndExactID(t *testing.T) {
	provider := newSubmitTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/seedbox/list" || r.URL.Query().Get("ids") != "wanted" {
			t.Errorf("request = %s?%s, want /seedbox/list?ids=wanted", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{
			"success":true,
			"value":[
				{"id":"other","name":"Wrong","status":100,"files":[]},
				{"id":"wanted","name":"Right","hashString":"hash","status":4,"downloadPercent":35,"totalSize":20,"created":1700000000,"files":[{"id":"file-1","name":"Right/video.mkv","size":20,"downloadUrl":"https://cdn.example/video","downloadPercent":35}]}
			]
		}`)
	})

	torrent, err := provider.GetTorrent("wanted")
	if err != nil {
		t.Fatal(err)
	}
	if torrent.Id != "wanted" || torrent.Name != "Right" || torrent.InfoHash != "hash" {
		t.Fatalf("torrent = %#v, want exact requested torrent", torrent)
	}
	if torrent.Status != types.TorrentStatusDownloading || torrent.Progress != 35 {
		t.Fatalf("status/progress = %q/%v, want downloading/35", torrent.Status, torrent.Progress)
	}
	file, ok := torrent.Files["video.mkv"]
	if !ok || file.DownloadLink.DownloadLink != "https://cdn.example/video" {
		t.Fatalf("file = %#v, exists=%v, want attached provider download link", file, ok)
	}
}

func TestUpdateTorrentSelectsRequestedID(t *testing.T) {
	provider := newSubmitTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/seedbox/list" || r.URL.Query().Get("ids") != "wanted" {
			t.Errorf("request = %s?%s, want /seedbox/list?ids=wanted", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"success":true,"value":[{"id":"other","name":"Wrong","status":100,"files":[]},{"id":"wanted","name":"Right","status":100,"downloadPercent":100,"files":[]}]}`)
	})

	torrent := &types.Torrent{Id: "wanted"}
	if err := provider.UpdateTorrent(torrent); err != nil {
		t.Fatal(err)
	}
	if torrent.Id != "wanted" || torrent.Name != "Right" || torrent.Status != types.TorrentStatusDownloaded {
		t.Fatalf("torrent = %#v, want exact requested completed torrent", torrent)
	}
}

func TestGetProfileTreatsPremiumLeftAsDuration(t *testing.T) {
	const premiumLeft = int64(3_600)
	provider := newSubmitTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/account/infos" {
			t.Errorf("path = %q, want /account/infos", r.URL.Path)
		}
		_, _ = fmt.Fprintf(w, `{"success":true,"value":{"username":"user","email":"user@example.com","accountType":1,"premiumLeft":%d,"pts":7}}`, premiumLeft)
	})

	before := time.Now()
	profile, err := provider.GetProfile()
	after := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	wantMin := before.Add(time.Duration(premiumLeft) * time.Second)
	wantMax := after.Add(time.Duration(premiumLeft) * time.Second)
	if profile.Expiration.Before(wantMin) || profile.Expiration.After(wantMax) {
		t.Fatalf("expiration = %s, want between %s and %s", profile.Expiration, wantMin, wantMax)
	}
	if profile.Type != "premium" || profile.Premium != profile.Expiration.Unix() {
		t.Fatalf("profile = %#v, want premium with absolute expiration", profile)
	}
}

func TestGetProfileLeavesFreeAccountExpirationUnknown(t *testing.T) {
	provider := newSubmitTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"success":true,"value":{"username":"free","premiumLeft":0}}`)
	})
	profile, err := provider.GetProfile()
	if err != nil {
		t.Fatal(err)
	}
	if profile.Type != "free" || !profile.Expiration.IsZero() || profile.Premium != 0 {
		t.Fatalf("profile = %#v, want free account with unknown expiration", profile)
	}
}

func TestPremiumExpirationRejectsDurationOverflow(t *testing.T) {
	_, err := debridLinkPremiumExpiration(time.Now(), debridLinkMaxPremiumSeconds+1)
	if err == nil || !strings.Contains(err.Error(), "invalid premium duration") {
		t.Fatalf("debridLinkPremiumExpiration() error = %v, want overflow rejection", err)
	}
}

func TestGetTorrentsRejectsMissingValue(t *testing.T) {
	provider := newSubmitTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"success":false}`)
	})
	_, err := provider.getTorrents(0, 100)
	if err == nil || !strings.Contains(err.Error(), "error getting torrents") {
		t.Fatalf("getTorrents() error = %v, want invalid envelope error", err)
	}
}

func TestTorrentLookupsRejectMissingIDs(t *testing.T) {
	provider := &DebridLink{}
	if _, err := provider.GetTorrent("   "); err == nil || !strings.Contains(err.Error(), "ID is empty") {
		t.Fatalf("GetTorrent() error = %v, want empty ID rejection", err)
	}
	if err := provider.UpdateTorrent(&types.Torrent{}); err == nil || !strings.Contains(err.Error(), "ID is missing") {
		t.Fatalf("UpdateTorrent() error = %v, want missing ID rejection", err)
	}
}
