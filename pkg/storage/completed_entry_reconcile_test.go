package storage

import (
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/internal/config"
	debridTypes "github.com/Trifocals3537/tessarr/pkg/debrid/types"
)

func TestReconcileCompletedTorrentEntryBeforeMergePreservesCanonicalIdentity(t *testing.T) {
	remoteFiles, err := debridTypes.FilesByLogicalName([]debridTypes.File{
		{Id: "one", Path: "Season 01/Episode.mkv", Size: 11, Link: "provider://one"},
		{Id: "two", Path: "Season 02/Episode.mkv", Size: 12, Link: "provider://two"},
	})
	if err != nil {
		t.Fatal(err)
	}
	completed := time.Now()
	const legacyName = "Season 01/Episode.mkv"
	existing := &Entry{
		Protocol:       config.ProtocolTorrent,
		InfoHash:       "same-hash",
		CompletedAt:    &completed,
		ActiveProvider: "primary",
		Files: map[string]*File{
			legacyName: {ID: "stable-canonical", Name: legacyName, Path: legacyName, Size: 11},
		},
		Providers: map[string]*ProviderEntry{
			"primary": {Provider: "primary", Files: map[string]*ProviderFile{
				legacyName: {Id: "one", Path: legacyName, Link: "provider://old"},
			}},
		},
	}
	incoming := &Entry{
		Protocol:       config.ProtocolTorrent,
		InfoHash:       existing.InfoHash,
		ActiveProvider: "primary",
		Files:          make(map[string]*File),
		Providers:      make(map[string]*ProviderEntry),
	}
	if _, err := incoming.AddTorrentProvider(&debridTypes.Torrent{
		Id: "transfer", Debrid: "primary", Status: debridTypes.TorrentStatusDownloaded, Files: remoteFiles,
	}); err != nil {
		t.Fatal(err)
	}
	for name, remote := range remoteFiles {
		incoming.Files[name] = newCanonicalTorrentFile(incoming, remote)
	}
	if err := ReconcileCompletedTorrentEntry(existing, incoming); err != nil {
		t.Fatal(err)
	}
	merged := HandleExistingEntryMerge(existing, incoming)
	if len(merged.Files) != 2 {
		t.Fatalf("queue merge created duplicate identities: %#v", merged.Files)
	}
	if merged.Files[legacyName] == nil || merged.Files[legacyName].ID != "stable-canonical" {
		t.Fatalf("legacy canonical identity was not preserved: %#v", merged.Files)
	}
	if merged.Providers["primary"].Files[legacyName] == nil {
		t.Fatalf("completed placement did not reconcile to legacy key: %#v", merged.Providers["primary"].Files)
	}
}
