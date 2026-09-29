package types

import (
	"path"
	"reflect"
	"strings"
	"testing"
)

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
	wantPaths := map[string]bool{
		"Release/Season 01/Episode.mkv": true,
		"Release/Season 02/Episode.mkv": true,
	}
	for name, file := range files {
		if file.Name != name || strings.ContainsAny(name, `/\`) || path.Ext(name) != ".mkv" {
			t.Fatalf("logical file %q = %#v", name, file)
		}
		if !wantPaths[file.Path] {
			t.Fatalf("unexpected provider path %q", file.Path)
		}
		delete(wantPaths, file.Path)
	}
	if len(wantPaths) != 0 {
		t.Fatalf("missing provider paths: %#v", wantPaths)
	}
}

func TestFilesByLogicalNameSanitizesPortablePunctuationAndDetectsCollisions(t *testing.T) {
	files, err := FilesByLogicalName([]File{{Id: "1", Path: "Release/Why?.mkv"}})
	if err != nil {
		t.Fatal(err)
	}
	if file, exists := files["Why_.mkv"]; !exists || file.Name != "Why_.mkv" {
		t.Fatalf("sanitized file = %#v, exists=%v", file, exists)
	}
	disambiguated, err := FilesByLogicalName([]File{
		{Id: "1", Path: "Release/Why?.mkv"},
		{Id: "2", Path: "Release/Why*.mkv"},
	})
	if err != nil {
		t.Fatalf("distinct unsafe basenames should be deterministically disambiguated: %v", err)
	}
	if len(disambiguated) != 2 {
		t.Fatalf("disambiguated file count = %d, want 2", len(disambiguated))
	}
	for name, file := range disambiguated {
		if name != file.Name || strings.ContainsAny(name, `/\?*`) || path.Ext(name) != ".mkv" {
			t.Fatalf("unsafe disambiguated file %q = %#v", name, file)
		}
	}
}

func TestFilesByLogicalNameIsDeterministicAcrossProviderOrder(t *testing.T) {
	firstInput := []File{
		{Id: "1", Path: "Release/Season 01/Episode.mkv"},
		{Id: "2", Path: "Release/Season 02/Episode.mkv"},
	}
	secondInput := []File{firstInput[1], firstInput[0]}
	first, err := FilesByLogicalName(firstInput)
	if err != nil {
		t.Fatal(err)
	}
	second, err := FilesByLogicalName(secondInput)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("logical names changed with provider order:\nfirst: %#v\nsecond: %#v", first, second)
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
