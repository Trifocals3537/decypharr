package manager

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestTorrentFileLayoutsPreserveUnambiguousNestedDuplicateBasenames(t *testing.T) {
	entry := torrentOwnershipTestEntry(t.TempDir(), "layout-owner", config.DownloadActionSymlink)
	entry.Files = map[string]*storage.File{
		"season-1": {
			Name: "episode.mkv",
			Path: "Release/Season 01/episode.mkv",
		},
		"season-2": {
			Name: "Season 02/episode.mkv",
			Path: "Release/Season 02/episode.mkv",
		},
	}
	layouts, err := torrentEntryFileLayouts(entry)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{layouts[0].relative, layouts[1].relative}
	want := []string{
		filepath.Join("Season 01", "episode.mkv"),
		filepath.Join("Season 02", "episode.mkv"),
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("layout %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestApplyDebridTorrentPreservesCollisionSafeLogicalNamesAndPaths(t *testing.T) {
	entry := &storage.Entry{
		InfoHash:  "torbox-logical-paths",
		Files:     make(map[string]*storage.File),
		Providers: make(map[string]*storage.ProviderEntry),
	}
	remote := &debridTypes.Torrent{
		Id:       "42",
		InfoHash: entry.InfoHash,
		Name:     "Release",
		Debrid:   "torbox",
		Files: map[string]debridTypes.File{
			"Release/Season 01/Episode.mkv": {
				Id:   "11",
				Name: "Release/Season 01/Episode.mkv",
				Path: "Release/Season 01/Episode.mkv",
				Link: "torbox://42/11",
			},
			"Release/Season 02/Episode.mkv": {
				Id:   "12",
				Name: "Release/Season 02/Episode.mkv",
				Path: "Release/Season 02/Episode.mkv",
				Link: "torbox://42/12",
			},
		},
	}

	applyDebridTorrentToEntry(entry, remote)

	if len(entry.Files) != 2 {
		t.Fatalf("managed file count = %d, want 2", len(entry.Files))
	}
	placement := entry.Providers["torbox"]
	if placement == nil || len(placement.Files) != 2 {
		t.Fatalf("provider files = %#v, want 2", placement)
	}
	for name, remoteFile := range remote.Files {
		managed := entry.Files[name]
		provider := placement.Files[name]
		if managed == nil || managed.Path != remoteFile.Path {
			t.Fatalf("managed file %q = %#v", name, managed)
		}
		if provider == nil || provider.Path != remoteFile.Path || provider.Id != remoteFile.Id {
			t.Fatalf("provider file %q = %#v", name, provider)
		}
	}
}

func TestTorrentFileLayoutsRejectTraversalAliasesAndAmbiguousBasenames(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]*storage.File
	}{
		{
			name: "traversal",
			files: map[string]*storage.File{
				"bad": {Name: "episode.mkv", Path: "../episode.mkv"},
			},
		},
		{
			name: "portable case collision",
			files: map[string]*storage.File{
				"a": {Name: "Season/Episode.mkv", Path: "Season/Episode.mkv"},
				"b": {Name: "season/episode.MKV", Path: "season/episode.MKV"},
			},
		},
		{
			name: "file directory prefix collision",
			files: map[string]*storage.File{
				"a": {Name: "Season", Path: "Season"},
				"b": {Name: "Season/Episode.mkv", Path: "Season/Episode.mkv"},
			},
		},
		{
			name: "duplicate logical key is ambiguous",
			files: map[string]*storage.File{
				"a": {Name: "episode.mkv", Path: "Season 01/episode.mkv"},
				"b": {Name: "episode.mkv", Path: "Season 02/episode.mkv"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry := torrentOwnershipTestEntry(t.TempDir(), "layout-"+strings.ReplaceAll(test.name, " ", "-"), config.DownloadActionSymlink)
			entry.Files = test.files
			if _, err := torrentEntryFileLayouts(entry); err == nil {
				t.Fatal("unsafe torrent layout was accepted")
			}
		})
	}
}

func TestTorrentFileLayoutsRejectInvalidOrUnboundedDownloadSizes(t *testing.T) {
	tests := []struct {
		name  string
		size  int64
		bytes *[2]int64
	}{
		{name: "negative size", size: -1},
		{name: "negative range", size: 1, bytes: &[2]int64{-1, 0}},
		{name: "inverted range", size: 1, bytes: &[2]int64{2, 1}},
		{name: "overflowing range", size: 1, bytes: &[2]int64{0, math.MaxInt64}},
		{name: "unknown download size", size: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry := torrentOwnershipTestEntry(t.TempDir(), "invalid-size-"+strings.ReplaceAll(test.name, " ", "-"), config.DownloadActionDownload)
			entry.Files["movie.mkv"].Size = test.size
			entry.Files["movie.mkv"].ByteRange = test.bytes
			if _, err := torrentEntryFileLayouts(entry); err == nil {
				t.Fatal("unsafe transfer metadata was accepted")
			}
		})
	}

	symlink := torrentOwnershipTestEntry(t.TempDir(), "unknown-symlink-size", config.DownloadActionSymlink)
	symlink.Files["movie.mkv"].Size = 0
	if _, err := torrentEntryFileLayouts(symlink); err != nil {
		t.Fatalf("symlink action rejected an unknown provider size: %v", err)
	}
}

func TestTorrentFileLayoutsEnforceEntryCountAndTotalTransferBounds(t *testing.T) {
	tooMany := torrentOwnershipTestEntry(t.TempDir(), "too-many-files", config.DownloadActionSymlink)
	tooMany.Files = make(map[string]*storage.File, torrentOwnershipMaxEntries+1)
	shared := &storage.File{Name: "movie.mkv", Path: "Release/movie.mkv"}
	for index := 0; index <= torrentOwnershipMaxEntries; index++ {
		tooMany.Files[strconv.Itoa(index)] = shared
	}
	if _, err := torrentEntryFileLayouts(tooMany); err == nil {
		t.Fatal("torrent file count above the bounded filesystem ceiling was accepted")
	}

	root := t.TempDir()
	overflow := torrentOwnershipTestEntry(root, "transfer-overflow", config.DownloadActionDownload)
	overflow.Files = map[string]*storage.File{
		"large.mkv": {Name: "large.mkv", Path: "Release/large.mkv", Size: math.MaxInt64},
		"extra.mkv": {Name: "extra.mkv", Path: "Release/extra.mkv", Size: 1},
	}
	downloader := &Downloader{dest: root, logger: zerolog.Nop()}
	err := downloader.processTorrentDownload(context.Background(), overflow)
	if err == nil || !strings.Contains(err.Error(), "total transfer size overflows") {
		t.Fatalf("overflow error = %v", err)
	}
	if _, err := os.Lstat(overflow.DownloadPath()); !os.IsNotExist(err) {
		t.Fatalf("overflowing transfer claimed an output directory: %v", err)
	}
}

func TestClaimTorrentEntryDirectoryWritesDurableOwnerAndRollsBackOnSyncFailure(t *testing.T) {
	root := t.TempDir()
	entry := torrentOwnershipTestEntry(root, "durable-owner", config.DownloadActionDownload)
	path, newlyClaimed, err := claimTorrentEntryDirectory(root, entry, torrentLegacyProof{})
	if err != nil {
		t.Fatal(err)
	}
	if !newlyClaimed {
		t.Fatal("new torrent release directory was not reported as newly claimed")
	}
	assertTorrentOwnerMarker(t, path, "durable-owner")

	failing := torrentOwnershipTestEntry(root, "sync-failure", config.DownloadActionDownload)
	failing.Name = "Other Release"
	originalSyncDirectory := torrentSyncDirectory
	torrentSyncDirectory = func(*os.File) error { return errors.New("injected directory sync failure") }
	t.Cleanup(func() { torrentSyncDirectory = originalSyncDirectory })
	if _, _, err := claimTorrentEntryDirectory(root, failing, torrentLegacyProof{}); err == nil {
		t.Fatal("ownership claim ignored directory sync failure")
	}
	if _, err := os.Lstat(filepath.Join(failing.DownloadPath(), torrentOwnerMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("failed ownership marker remained after sync failure: %v", err)
	}
}

func TestClaimTorrentEntryDirectoryConservativelyAdoptsExactLegacySymlinks(t *testing.T) {
	root := t.TempDir()
	mount := t.TempDir()
	entry := torrentOwnershipTestEntry(root, "legacy-symlink", config.DownloadActionSymlink)
	entry.Files = map[string]*storage.File{
		"nested": {
			Name: "Season 01/episode.mkv",
			Path: "Release/Season 01/episode.mkv",
		},
	}
	relative := filepath.Join("Season 01", "episode.mkv")
	target := filepath.Join(mount, relative)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(entry.DownloadPath(), relative)
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, output); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, newlyClaimed, err := claimTorrentEntryDirectory(root, entry, torrentLegacyProof{mountPath: mount}); err != nil {
		t.Fatal(err)
	} else if !newlyClaimed {
		t.Fatal("exact legacy directory was not adopted")
	}
	assertTorrentOwnerMarker(t, entry.DownloadPath(), "legacy-symlink")

	wrong := torrentOwnershipTestEntry(root, "wrong-legacy", config.DownloadActionSymlink)
	wrong.Name = "Wrong Release"
	wrong.Files = map[string]*storage.File{
		"nested": {
			Name: "Season 01/episode.mkv",
			Path: "Wrong Release/Season 01/episode.mkv",
		},
	}
	wrongOutput := filepath.Join(wrong.DownloadPath(), relative)
	if err := os.MkdirAll(filepath.Dir(wrongOutput), 0o700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "unrelated.mkv")
	if err := os.WriteFile(external, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, wrongOutput); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := claimTorrentEntryDirectory(root, wrong, torrentLegacyProof{mountPath: mount}); err == nil ||
		!strings.Contains(err.Error(), "manual review") {
		t.Fatalf("wrong-target legacy directory error = %v", err)
	}
	if got, err := os.Readlink(wrongOutput); err != nil || got != external {
		t.Fatalf("wrong-target legacy link changed: target=%q error=%v", got, err)
	}
}

func TestRemoveOwnedTorrentDirectoryRequiresMarkerAndRecoversQuarantine(t *testing.T) {
	root := t.TempDir()
	unowned := torrentOwnershipTestEntry(root, "unowned", config.DownloadActionDownload)
	if err := os.MkdirAll(unowned.DownloadPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(unowned.DownloadPath(), "keep")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedTorrentEntryDirectory(root, unowned); err == nil {
		t.Fatal("unowned torrent directory was accepted for deletion")
	}
	assertTorrentTestContents(t, sentinel, "keep")

	owned := torrentOwnershipTestEntry(root, "quarantine-owner", config.DownloadActionDownload)
	owned.Name = "Owned Release"
	ownedPath, _, err := claimTorrentEntryDirectory(root, owned, torrentLegacyProof{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownedPath, "payload"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalHook := torrentAfterQuarantine
	torrentAfterQuarantine = func(string) error { return errors.New("simulated crash") }
	t.Cleanup(func() { torrentAfterQuarantine = originalHook })
	if err := removeOwnedTorrentEntryDirectory(root, owned); err == nil {
		t.Fatal("simulated crash did not interrupt quarantine cleanup")
	}
	if _, err := os.Lstat(ownedPath); !os.IsNotExist(err) {
		t.Fatalf("visible owned path remained after quarantine: %v", err)
	}
	torrentAfterQuarantine = nil
	if err := removeOwnedTorrentEntryDirectory(root, owned); err != nil {
		t.Fatalf("quarantine recovery failed: %v", err)
	}
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(ownedPath), torrentQuarantinePrefix+"*")); err != nil || len(matches) != 0 {
		t.Fatalf("quarantine remnants = %v, error=%v", matches, err)
	}
}

func TestRemoveOwnedTorrentDirectoryPreservesPathSwapReplacement(t *testing.T) {
	root := t.TempDir()
	entry := torrentOwnershipTestEntry(root, "path-swap-owner", config.DownloadActionDownload)
	path, _, err := claimTorrentEntryDirectory(root, entry, torrentLegacyProof{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "owned"), []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacementSentinel := filepath.Join(path, "replacement")
	originalHook := torrentAfterQuarantine
	torrentAfterQuarantine = func(visible string) error {
		if err := os.MkdirAll(visible, 0o700); err != nil {
			return err
		}
		return os.WriteFile(replacementSentinel, []byte("preserve"), 0o600)
	}
	t.Cleanup(func() { torrentAfterQuarantine = originalHook })
	if err := removeOwnedTorrentEntryDirectory(root, entry); err != nil {
		t.Fatal(err)
	}
	assertTorrentTestContents(t, replacementSentinel, "preserve")
	if err := removeOwnedTorrentEntryDirectory(root, entry); err == nil {
		t.Fatal("unowned path-swap replacement was accepted on retry")
	}
	assertTorrentTestContents(t, replacementSentinel, "preserve")
}

func TestRemoveOwnedTorrentDirectoryNeverRecursesIntoVerifiedQuarantineReplacement(t *testing.T) {
	root := t.TempDir()
	entry := torrentOwnershipTestEntry(root, "quarantine-swap-owner", config.DownloadActionDownload)
	ownedPath, _, err := claimTorrentEntryDirectory(root, entry, torrentLegacyProof{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownedPath, "owned"), []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}

	var movedOwned string
	var replacementSentinel string
	originalHook := torrentAfterQuarantineVerified
	torrentAfterQuarantineVerified = func(quarantineRelative string) error {
		quarantine := filepath.Join(root, quarantineRelative)
		movedOwned = quarantine + "-moved-owned"
		if err := os.Rename(quarantine, movedOwned); err != nil {
			return err
		}
		if err := os.Mkdir(quarantine, 0o700); err != nil {
			return err
		}
		replacementSentinel = filepath.Join(quarantine, "preserve")
		return os.WriteFile(replacementSentinel, []byte("replacement"), 0o600)
	}
	t.Cleanup(func() { torrentAfterQuarantineVerified = originalHook })

	err = removeOwnedTorrentEntryDirectory(root, entry)
	if err == nil || !strings.Contains(err.Error(), "changed before marker removal") {
		t.Fatalf("quarantine replacement error = %v", err)
	}
	assertTorrentTestContents(t, replacementSentinel, "replacement")
	assertTorrentOwnerMarker(t, movedOwned, "quarantine-swap-owner")
	if _, err := os.Lstat(filepath.Join(movedOwned, "owned")); !os.IsNotExist(err) {
		t.Fatalf("pinned owned content remained after cleanup: %v", err)
	}
}

func TestTorrentOwnerMarkerSwapIsRejected(t *testing.T) {
	root := t.TempDir()
	entry := torrentOwnershipTestEntry(root, "marker-swap-owner", config.DownloadActionDownload)
	path, _, err := claimTorrentEntryDirectory(root, entry, torrentLegacyProof{})
	if err != nil {
		t.Fatal(err)
	}
	originalHook := torrentAfterMarkerLstat
	called := false
	torrentAfterMarkerLstat = func(root *os.Root) error {
		if called {
			return nil
		}
		called = true
		if err := root.Rename(torrentOwnerMarkerName, torrentOwnerMarkerName+".old"); err != nil {
			return err
		}
		return root.WriteFile(torrentOwnerMarkerName, []byte("marker-swap-owner\n"), 0o600)
	}
	t.Cleanup(func() { torrentAfterMarkerLstat = originalHook })
	if err := removeOwnedTorrentEntryDirectory(root, entry); err == nil ||
		!strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("marker swap error = %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("release directory was removed after marker swap: %v", err)
	}
}

func TestOwnedTorrentPartRejectsHardlinkedRecovery(t *testing.T) {
	root := t.TempDir()
	entry := torrentOwnershipTestEntry(root, "part-hardlink", config.DownloadActionDownload)
	if _, _, err := claimTorrentEntryDirectory(root, entry, torrentLegacyProof{}); err != nil {
		t.Fatal(err)
	}
	part, err := openOwnedTorrentPart(root, entry, "movie.mkv", 4)
	if err != nil {
		t.Fatal(err)
	}
	partPath := part.partAbsolutePath
	if _, err := part.file.Write([]byte("da")); err != nil {
		t.Fatal(err)
	}
	if err := part.Close(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-link")
	if err := os.Link(partPath, outside); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := openOwnedTorrentPart(root, entry, "movie.mkv", 4); err == nil {
		t.Fatal("hardlinked recovered partial file was accepted")
	}
	assertTorrentTestContents(t, outside, "da")
}

func TestOwnedTorrentPartRecoversPublishedCrashHardLinks(t *testing.T) {
	root := t.TempDir()
	entry := torrentOwnershipTestEntry(root, "part-published-crash", config.DownloadActionDownload)
	if _, _, err := claimTorrentEntryDirectory(root, entry, torrentLegacyProof{}); err != nil {
		t.Fatal(err)
	}
	part, err := openOwnedTorrentPart(root, entry, "movie.mkv", 4)
	if err != nil {
		t.Fatal(err)
	}
	partPath := part.partAbsolutePath
	if _, err := part.file.Write([]byte("good")); err != nil {
		t.Fatal(err)
	}
	if err := part.Close(); err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(entry.DownloadPath(), "movie.mkv")
	if err := os.Link(partPath, finalPath); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}

	recovered, err := openOwnedTorrentPart(root, entry, "movie.mkv", 4)
	if err != nil {
		t.Fatalf("valid two-link published crash state was rejected: %v", err)
	}
	if err := recovered.Commit(); err != nil {
		_ = recovered.Close()
		t.Fatalf("commit recovered published crash state: %v", err)
	}
	if _, err := os.Lstat(partPath); !os.IsNotExist(err) {
		t.Fatalf("published crash partial link remained: %v", err)
	}
	assertTorrentTestContents(t, finalPath, "good")
}

func TestOwnedTorrentPartCommitNeverOverwritesWrongFinal(t *testing.T) {
	tests := []string{"regular", "symlink", "directory"}
	for _, kind := range tests {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			entry := torrentOwnershipTestEntry(root, "publish-"+kind, config.DownloadActionDownload)
			if _, _, err := claimTorrentEntryDirectory(root, entry, torrentLegacyProof{}); err != nil {
				t.Fatal(err)
			}
			part, err := openOwnedTorrentPart(root, entry, "movie.mkv", 4)
			if err != nil {
				t.Fatal(err)
			}
			defer part.Close()
			if _, err := part.file.Write([]byte("good")); err != nil {
				t.Fatal(err)
			}
			final := filepath.Join(entry.DownloadPath(), "movie.mkv")
			switch kind {
			case "regular":
				if err := os.WriteFile(final, []byte("wrong"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				external := filepath.Join(t.TempDir(), "external")
				if err := os.WriteFile(external, []byte("wrong"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, final); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "directory":
				if err := os.Mkdir(final, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := part.Commit(); err == nil {
				t.Fatal("wrong existing final artifact was overwritten")
			}
			info, err := os.Lstat(final)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "regular":
				assertTorrentTestContents(t, final, "wrong")
			case "symlink":
				if info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("existing final symlink was replaced")
				}
			case "directory":
				if !info.IsDir() {
					t.Fatal("existing final directory was replaced")
				}
			}
		})
	}
}

func TestWriteOwnedTorrentFileUsesNoOverwriteIdempotence(t *testing.T) {
	root := t.TempDir()
	entry := torrentOwnershipTestEntry(root, "strm-no-overwrite", config.DownloadActionStrm)
	if _, _, err := claimTorrentEntryDirectory(root, entry, torrentLegacyProof{}); err != nil {
		t.Fatal(err)
	}
	relative := filepath.Join("Season 01", "episode.strm")
	if err := writeOwnedTorrentFile(root, entry, relative, []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeOwnedTorrentFile(root, entry, relative, []byte("same"), 0o600); err != nil {
		t.Fatalf("exact idempotent STRM retry failed: %v", err)
	}
	if err := writeOwnedTorrentFile(root, entry, relative, []byte("wrong"), 0o600); err == nil {
		t.Fatal("wrong existing STRM artifact was replaced")
	}
	assertTorrentTestContents(t, filepath.Join(entry.DownloadPath(), relative), "same")
}

func torrentOwnershipTestEntry(root, owner string, action config.DownloadAction) *storage.Entry {
	return &storage.Entry{
		Protocol: config.ProtocolTorrent,
		InfoHash: owner,
		Name:     "Release",
		SavePath: filepath.Join(root, "radarr"),
		Action:   action,
		Files: map[string]*storage.File{
			"movie.mkv": {
				Name: "movie.mkv",
				Path: "Release/movie.mkv",
				Size: 4,
			},
		},
	}
}

func assertTorrentOwnerMarker(t *testing.T, entryPath, want string) {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(entryPath, torrentOwnerMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != want+"\n" {
		t.Fatalf("owner marker = %q, want %q", contents, want+"\n")
	}
}

func assertTorrentTestContents(t *testing.T, path, want string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != want {
		t.Fatalf("%q contents = %q, want %q", path, contents, want)
	}
}
