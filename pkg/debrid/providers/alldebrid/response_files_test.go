package alldebrid

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
)

func TestDoRequestRejectsTrailingJSON(t *testing.T) {
	config.SetConfigPath(t.TempDir())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{} {}`))
	}))
	defer server.Close()

	client := &AllDebrid{Host: server.URL, client: request.New()}
	var result map[string]any
	if _, err := client.doRequest("/response", nil, &result); err == nil ||
		!strings.Contains(err.Error(), "multiple values") {
		t.Fatalf("doRequest error = %v, want trailing JSON rejection", err)
	}
}

func TestFlattenFilesPreservesNestedDuplicateBasenames(t *testing.T) {
	files, err := (&AllDebrid{}).flattenFiles("ad", []MagnetFile{
		{
			Name: "Season 01",
			Elements: []MagnetFile{{
				Name: "Episode.mkv",
				Size: 1024,
				Link: "https://download.invalid/1",
			}},
		},
		{
			Name: "Season 02",
			Elements: []MagnetFile{{
				Name: "Episode.mkv",
				Size: 2048,
				Link: "https://download.invalid/2",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Season 01/Episode.mkv", "Season 02/Episode.mkv"} {
		if file, exists := files[name]; !exists || file.Name != name || file.Path != name {
			t.Fatalf("file %q = %#v, exists=%v", name, file, exists)
		}
	}
}

func TestFlattenFilesRejectsExcessiveNesting(t *testing.T) {
	tree := MagnetFile{Name: "Episode.mkv", Size: 1, Link: "https://download.invalid"}
	for depth := 0; depth <= allDebridFileTreeMaxDepth; depth++ {
		tree = MagnetFile{Name: "folder", Elements: []MagnetFile{tree}}
	}
	if _, err := (&AllDebrid{}).flattenFiles("ad", []MagnetFile{tree}); err == nil {
		t.Fatal("expected excessive AllDebrid nesting to be rejected")
	}
}
