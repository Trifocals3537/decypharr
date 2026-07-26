package torbox

import (
	"testing"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestTorboxFilesByLogicalNamePreservesNestedDuplicateBasenames(t *testing.T) {
	files, err := torboxFilesByLogicalName([]types.File{
		{Id: "11", Path: "Release/Season 01/Episode.mkv"},
		{Id: "12", Path: "Release/Season 02/Episode.mkv"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 2 {
		t.Fatalf("file count = %d, want 2", len(files))
	}
	for _, name := range []string{
		"Release/Season 01/Episode.mkv",
		"Release/Season 02/Episode.mkv",
	} {
		file, ok := files[name]
		if !ok {
			t.Fatalf("missing collision-safe logical name %q in %#v", name, files)
		}
		if file.Name != name || file.Path != name {
			t.Fatalf("file %q = %#v, want matching logical name and provider path", name, file)
		}
	}
}

func TestTorboxFilesByLogicalNameKeepsUniqueBasenameCompatibility(t *testing.T) {
	files, err := torboxFilesByLogicalName([]types.File{
		{Id: "21", Path: `Release\Season 01\Unique.mkv`},
	})
	if err != nil {
		t.Fatal(err)
	}

	file, ok := files["Unique.mkv"]
	if !ok {
		t.Fatalf("unique basename key missing in %#v", files)
	}
	if file.Name != "Unique.mkv" || file.Path != "Release/Season 01/Unique.mkv" {
		t.Fatalf("unique file = %#v", file)
	}
}

func TestTorboxFilesByLogicalNameRejectsDuplicateProviderPath(t *testing.T) {
	_, err := torboxFilesByLogicalName([]types.File{
		{Id: "31", Path: "Release/Episode.mkv"},
		{Id: "32", Path: "Release/Episode.mkv"},
	})
	if err == nil {
		t.Fatal("duplicate provider path was accepted")
	}
}
