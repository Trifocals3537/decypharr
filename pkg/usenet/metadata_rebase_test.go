package usenet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Trifocals3537/tessarr/pkg/storage"
)

func TestRebaseNZBMetadataPaths(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "old")
	target := filepath.Join(root, "new")
	metaDir := filepath.Join(root, "stage", "usenet", "meta")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ids := []string{
		"123e4567-e89b-12d3-a456-426614174000",
		"123e4567-e89b-12d3-a456-426614174001",
	}
	for index, id := range ids {
		path := filepath.Join(source, "usenet", "nzbs", id+".nzb")
		if index == 1 {
			path = filepath.Join(root, "external", id+".nzb")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("nzb"), 0o600); err != nil {
			t.Fatal(err)
		}
		encoded, err := encodeNZBV2(&storage.NZB{ID: id, Name: "Episode", Path: path})
		if err != nil {
			t.Fatal(err)
		}
		metaPath, err := metadataFilePath(metaDir, id, nzbMetaSuffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeMetadataFile(metaDir, metaPath, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	count, err := RebaseNZBMetadataPaths(metaDir, source, target)
	if err != nil || count != 1 {
		t.Fatalf("RebaseNZBMetadataPaths() = %d, %v; want 1, nil", count, err)
	}
	for index, id := range ids {
		metaPath, err := metadataFilePath(metaDir, id, nzbMetaSuffix)
		if err != nil {
			t.Fatal(err)
		}
		data, err := readMetadataFile(metaDir, metaPath)
		if err != nil {
			t.Fatal(err)
		}
		nzb, err := decodeNZB(data)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(target, "usenet", "nzbs", id+".nzb")
		if index == 1 {
			want = filepath.Join(root, "external", id+".nzb")
		}
		if nzb.Path != want {
			t.Fatalf("metadata %s path = %q, want %q", id, nzb.Path, want)
		}
	}
}

func TestRebaseNZBMetadataPathsRejectsCrossEntrySource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "old")
	metaDir := filepath.Join(root, "stage", "usenet", "meta")
	if err := os.MkdirAll(filepath.Join(source, "usenet", "nzbs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "123e4567-e89b-12d3-a456-426614174000"
	wrong := filepath.Join(source, "usenet", "nzbs", "123e4567-e89b-12d3-a456-426614174001.nzb")
	encoded, err := encodeNZBV2(&storage.NZB{ID: id, Path: wrong})
	if err != nil {
		t.Fatal(err)
	}
	metaPath, err := metadataFilePath(metaDir, id, nzbMetaSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMetadataFile(metaDir, metaPath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RebaseNZBMetadataPaths(metaDir, source, filepath.Join(root, "new")); err == nil || !strings.Contains(err.Error(), "unsafe stored source path") {
		t.Fatalf("cross-entry source path error = %v", err)
	}
}
