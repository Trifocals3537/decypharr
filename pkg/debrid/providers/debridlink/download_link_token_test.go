package debridlink

import (
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestFetchDownloadLinkTracksDownloadAccountToken(t *testing.T) {
	provider := &DebridLink{
		APIKey:                "main-token",
		autoExpiresLinksAfter: time.Hour,
		config:                config.Debrid{Name: "debridlink"},
	}
	got, err := provider.fetchDownloadLink(
		&account.Account{Token: "download-token"},
		"42",
		&types.File{Link: "https://cdn.example/video", Name: "video.mkv", Size: 4},
	)
	if err != nil {
		t.Fatalf("fetchDownloadLink() error = %v", err)
	}
	if got.Token != "download-token" {
		t.Fatalf("link token = %q, want owning download token", got.Token)
	}
}
