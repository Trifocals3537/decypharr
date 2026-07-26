package types

import "testing"

func TestFilesByLogicalNamePreservesUniqueBasenameCompatibility(t *testing.T) {
	files, err := FilesByLogicalName([]File{{
		Id:   "1",
		Name: "ignored-name",
		Path: "/Release/Season 01/Episode 01.mkv",
	}})
	if err != nil {
		t.Fatal(err)
	}
	file, exists := files["Episode 01.mkv"]
	if !exists {
		t.Fatalf("files = %#v, want historical basename key", files)
	}
	if file.Name != "Episode 01.mkv" {
		t.Fatalf("logical name = %q", file.Name)
	}
	if file.Path != "Release/Season 01/Episode 01.mkv" {
		t.Fatalf("provider path = %q", file.Path)
	}
}

func TestFilesByLogicalNamePreservesNestedDuplicateBasenames(t *testing.T) {
	files, err := FilesByLogicalName([]File{
		{Id: "1", Path: `Release\Season 01\Episode.mkv`},
		{Id: "2", Path: "Release/Season 02/Episode.mkv"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"Release/Season 01/Episode.mkv",
		"Release/Season 02/Episode.mkv",
	} {
		file, exists := files[name]
		if !exists || file.Name != name || file.Path != name {
			t.Fatalf("file %q = %#v, exists=%v", name, file, exists)
		}
	}
}

func TestFilesByLogicalNameFailsClosedOnPortablePathCollision(t *testing.T) {
	_, err := FilesByLogicalName([]File{
		{Id: "1", Path: "Release/Season/Episode.mkv"},
		{Id: "2", Path: "release/season/episode.MKV"},
	})
	if err == nil {
		t.Fatal("expected portable path collision to be rejected")
	}
}

func TestFilesByLogicalNameRejectsTraversal(t *testing.T) {
	if _, err := FilesByLogicalName([]File{{Path: "../escape.mkv"}}); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}

func TestFilesByLogicalNameRejectsUnboundedFileSets(t *testing.T) {
	if _, err := FilesByLogicalName(make([]File, maxProviderFileRecords+1)); err == nil {
		t.Fatal("expected oversized provider file set to be rejected")
	}
}
