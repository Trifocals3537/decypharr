package debridlink

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
)

func TestDoGetRejectsTrailingJSON(t *testing.T) {
	config.SetConfigPath(t.TempDir())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{} {}`))
	}))
	defer server.Close()

	client := &DebridLink{Host: server.URL, client: request.New()}
	var result map[string]any
	if _, err := client.doGet("/response", nil, &result); err == nil ||
		!strings.Contains(err.Error(), "multiple values") {
		t.Fatalf("doGet error = %v, want trailing JSON rejection", err)
	}
}

func TestFilesByLogicalNamePreservesNestedDuplicateBasenames(t *testing.T) {
	files, err := (&DebridLink{}).filesByLogicalName("dl", []torrentFile{
		{
			ID:          "1",
			Name:        "Release/Season 01/Episode.mkv",
			DownloadURL: "https://download.invalid/1",
			Size:        1024,
		},
		{
			ID:          "2",
			Name:        "Release/Season 02/Episode.mkv",
			DownloadURL: "https://download.invalid/2",
			Size:        2048,
		},
	})
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
