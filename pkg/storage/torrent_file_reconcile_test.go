package storage

import (
	"testing"

	debridTypes "github.com/Trifocals3537/tessarr/pkg/debrid/types"
)

func TestAddTorrentProviderReconcilesPersistedLogicalNames(t *testing.T) {
	const oldName = "Release/Season 01/Episode.mkv"
	remoteFiles, err := debridTypes.FilesByLogicalName([]debridTypes.File{
		{Id: "stable-1", Path: "Release/Season 01/Episode.mkv", Size: 11},
		{Id: "stable-2", Path: "Release/Season 02/Episode.mkv", Size: 12},
	})
	if err != nil {
		t.Fatal(err)
	}
	var newName string
	for name, file := range remoteFiles {
		if file.Id == "stable-1" {
			newName = name
		}
	}
	if newName == "" || newName == oldName {
		t.Fatalf("new logical name = %q", newName)
	}

	persisted := &File{ID: "file-id", Name: oldName, Path: "Release/Season 01/Episode.mkv", Size: 10}
	entry := &Entry{
		Files: map[string]*File{oldName: persisted},
		Providers: map[string]*ProviderEntry{
			"primary": {
				Provider: "primary",
				Files: map[string]*ProviderFile{
					oldName: {Id: "stable-1", Path: "Release/Season 01/Episode.mkv"},
				},
			},
			"fallback": {
				Provider: "fallback",
				Files: map[string]*ProviderFile{
					oldName: {Id: "other-id", Path: "Release/Season 01/Episode.mkv"},
				},
			},
		},
	}
	remote := &debridTypes.Torrent{Id: "placement", Debrid: "primary", Files: remoteFiles}
	if _, err := entry.AddTorrentProvider(remote); err != nil {
		t.Fatal(err)
	}
	if entry.Files[oldName] != nil || entry.Files[newName] != persisted {
		t.Fatalf("canonical files = %#v", entry.Files)
	}
	if persisted.ID != "file-id" || persisted.Name != newName || persisted.Path != remoteFiles[newName].LocalPath() {
		t.Fatalf("reconciled canonical file = %#v", persisted)
	}
	if entry.Providers["primary"].Files[newName] == nil || entry.Providers["primary"].Files[oldName] != nil {
		t.Fatalf("primary placement files = %#v", entry.Providers["primary"].Files)
	}
	if entry.Providers["fallback"].Files[newName] == nil || entry.Providers["fallback"].Files[oldName] != nil {
		t.Fatalf("fallback placement files = %#v", entry.Providers["fallback"].Files)
	}
}

func TestAddTorrentProviderReconcilesByPathWithoutProviderID(t *testing.T) {
	remoteFiles, err := debridTypes.FilesByLogicalName([]debridTypes.File{{
		Path: "Release/Why?.mkv",
		Size: 42,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var newName string
	for name := range remoteFiles {
		newName = name
	}
	entry := &Entry{
		Files: map[string]*File{"Why_.mkv": {Name: "Why_.mkv", Path: "Release/Why?.mkv"}},
		Providers: map[string]*ProviderEntry{
			"primary": {
				Provider: "primary",
				Files: map[string]*ProviderFile{
					"Why_.mkv": {Path: `release\why?.mkv`},
				},
			},
		},
	}
	if _, err := entry.AddTorrentProvider(&debridTypes.Torrent{Debrid: "primary", Files: remoteFiles}); err != nil {
		t.Fatal(err)
	}
	if entry.Files[newName] == nil || entry.Files["Why_.mkv"] != nil {
		t.Fatalf("canonical files = %#v", entry.Files)
	}
}

func TestAddTorrentProviderRejectsCanonicalRenameCollision(t *testing.T) {
	remoteFiles, err := debridTypes.FilesByLogicalName([]debridTypes.File{{
		Id:   "stable",
		Path: "Release/Why?.mkv",
	}})
	if err != nil {
		t.Fatal(err)
	}
	var newName string
	for name := range remoteFiles {
		newName = name
	}
	entry := &Entry{
		Files: map[string]*File{
			"Why_.mkv": {Name: "Why_.mkv"},
			newName:    {Name: newName},
		},
		Providers: map[string]*ProviderEntry{
			"primary": {
				Provider: "primary",
				Files: map[string]*ProviderFile{
					"Why_.mkv": {Id: "stable", Path: "Release/Why?.mkv"},
				},
			},
		},
	}
	if _, err := entry.AddTorrentProvider(&debridTypes.Torrent{Debrid: "primary", Files: remoteFiles}); err == nil {
		t.Fatal("canonical rename collision was accepted")
	}
	if entry.Files["Why_.mkv"] == nil || entry.Providers["primary"].Files["Why_.mkv"] == nil {
		t.Fatal("failed reconciliation mutated the persisted entry")
	}
}

func TestAddTorrentProviderAppliesRenameChainsAtomically(t *testing.T) {
	first := &File{ID: "first", Name: "old-a.mkv"}
	second := &File{ID: "second", Name: "new-a.mkv"}
	entry := &Entry{
		Files: map[string]*File{
			"old-a.mkv": first,
			"new-a.mkv": second,
		},
		Providers: map[string]*ProviderEntry{
			"primary": {
				Provider: "primary",
				Files: map[string]*ProviderFile{
					"old-a.mkv": {Id: "provider-a"},
					"new-a.mkv": {Id: "provider-b"},
				},
			},
		},
	}
	remote := &debridTypes.Torrent{
		Debrid: "primary",
		Files: map[string]debridTypes.File{
			"new-a.mkv":   {Id: "provider-a", Name: "new-a.mkv", Path: "new-a.mkv"},
			"final-b.mkv": {Id: "provider-b", Name: "final-b.mkv", Path: "final-b.mkv"},
		},
	}
	if _, err := entry.AddTorrentProvider(remote); err != nil {
		t.Fatal(err)
	}
	if len(entry.Files) != 2 || entry.Files["new-a.mkv"] != first || entry.Files["final-b.mkv"] != second {
		t.Fatalf("atomic rename chain = %#v", entry.Files)
	}
}
