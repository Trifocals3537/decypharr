package manager

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestTorrentOutputPathMountSourceRemainsPortable(t *testing.T) {
	for _, naming := range []config.WebDavFolderNaming{
		"", config.WebDavUseFileName, config.WebDavUseOriginalName,
		config.WebDavUseFileNameNoExt, config.WebDavUseOriginalNameNoExt,
	} {
		t.Run(string(naming), func(t *testing.T) {
			for _, name := range []string{"Movie: Part Two", "CON", "movie?", "movie<", "movie>", "movie\"", "movie|", "movie*", "trailing.", "trailing ", "a/../movie", `a\movie`} {
				entry := &storage.Entry{
					Name: name, OriginalFilename: name, InfoHash: "safe-hash",
					OutputName: storage.NewTorrentOutputName(name, "safe-hash"),
				}
				for _, action := range []config.DownloadAction{config.DownloadActionSymlink, "", "unknown"} {
					entry.Action = action
					if err := validateTorrentSourceFolder(entry, naming, false); err == nil {
						t.Errorf("accepted unsafe %q mount source %q with action %q", naming, name, action)
					}
				}
			}
			entry := &storage.Entry{Name: "Movie.mkv", OriginalFilename: "Original.mkv", Action: config.DownloadActionSymlink}
			if err := validateTorrentSourceFolder(entry, naming, false); err != nil {
				t.Fatalf("portable source rejected: %v", err)
			}
		})
	}
	entry := &storage.Entry{Name: "Movie: Part Two", OriginalFilename: "Original?", InfoHash: "safe-hash", Action: config.DownloadActionSymlink}
	if err := validateTorrentSourceFolder(entry, config.WebdavUseHash, false); err != nil {
		t.Fatalf("safe hash source rejected because of unused titles: %v", err)
	}
	entry.InfoHash = "../escape"
	if err := validateTorrentSourceFolder(entry, config.WebdavUseHash, false); err == nil {
		t.Fatal("unsafe hash source accepted")
	}
	for _, action := range []config.DownloadAction{config.DownloadActionDownload, config.DownloadActionStrm, config.DownloadActionNone} {
		entry.Action = action
		if err := validateTorrentSourceFolder(entry, config.WebDavUseFileName, false); err != nil {
			t.Fatalf("mount-free action %q rejected a display title: %v", action, err)
		}
	}
}

func TestTorrentOutputPathValidatesSourceAtAdmissionAndProviderUpdate(t *testing.T) {
	request := asyncAdmissionTestRequest(t.TempDir(), "source-check", nil)
	request.Action = config.DownloadActionSymlink
	request.Magnet.Name = "Movie: Part Two"
	manager := &Manager{config: &config.Config{DownloadFolder: request.DownloadFolder}}
	if err := manager.validateTorrentImportRequest(request); err == nil {
		t.Fatal("unsafe title-based symlink source admitted")
	}
	request.Action = config.DownloadActionDownload
	if err := manager.validateTorrentImportRequest(request); err != nil {
		t.Fatalf("mount-free download rejected: %v", err)
	}
	request.Action = config.DownloadActionSymlink
	request.Magnet.Name = ""
	if err := manager.validateTorrentImportRequest(request); err != nil {
		t.Fatalf("nameless magnet rejected before provider resolution: %v", err)
	}
	resolved := &debridTypes.Torrent{Name: "Movie: Part Two", OriginalFilename: "Original.mkv", InfoHash: "safe-hash"}
	if err := manager.validateResolvedTorrentNames(resolved, request.Action); err == nil {
		t.Fatal("unsafe provider-updated symlink source accepted")
	}
	manager.config.FolderNaming = config.WebDavUseOriginalName
	if err := manager.validateResolvedTorrentNames(resolved, request.Action); err != nil {
		t.Fatalf("portable selected original filename rejected: %v", err)
	}
	resolved.Name, resolved.OriginalFilename = "Movie.mkv", "CON"
	if err := manager.validateResolvedTorrentNames(resolved, request.Action); err == nil {
		t.Fatal("unsafe provider original filename accepted")
	}
	resolved.OriginalFilename = ""
	if err := manager.validateResolvedTorrentNames(resolved, request.Action); err == nil {
		t.Fatal("missing resolved source accepted")
	}
	manager.config.FolderNaming = config.WebdavUseHash
	if err := manager.validateResolvedTorrentNames(resolved, request.Action); err != nil {
		t.Fatalf("hash source incorrectly required an original filename: %v", err)
	}
}

func TestTorrentOutputPathRechecksSourceBeforeLocalWork(t *testing.T) {
	entry := torrentOwnershipTestEntry(t.TempDir(), "../unsafe-hash", config.DownloadActionSymlink)
	entry.Name, entry.OriginalFilename = "CON", "CON"
	entry.OutputName = storage.NewTorrentOutputName(entry.Name, entry.InfoHash)
	// No manager/queue is needed: rejection must happen before any queue or
	// filesystem side effects, for every current mount-naming policy.
	downloader := &Downloader{}
	if err := downloader.download(context.Background(), entry); err == nil {
		t.Fatal("unsafe persisted source reached local processing")
	}
	if entry.IsDownloading {
		t.Fatal("source validation changed queue state")
	}
}

func TestTorrentOutputPathPreservesLegacyLeadingWhitespace(t *testing.T) {
	root := t.TempDir()
	entry := torrentOwnershipTestEntry(root, "legacy-spaces", config.DownloadActionDownload)
	entry.Name = " Release.mkv"
	want := filepath.Join(entry.SavePath, " Release")
	got, err := safeTorrentEntryDownloadPath(root, entry)
	if err != nil {
		t.Fatal(err)
	}
	if got != want || got != entry.DownloadPath() {
		t.Fatalf("legacy path = %q, want unchanged %q", got, want)
	}
	if _, _, err := claimTorrentEntryDirectory(root, entry, torrentLegacyProof{}); err != nil {
		t.Fatal(err)
	}
	if err := writeOwnedTorrentFile(root, entry, "movie.mkv", []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertTorrentTestContents(t, filepath.Join(want, "movie.mkv"), "data")
	if err := removeOwnedTorrentEntryDirectory(root, entry); err != nil {
		t.Fatal(err)
	}
}

func TestTorrentOutputPathAdmissionKeepsDisplayTitleAndStableProviderIdentity(t *testing.T) {
	request := asyncAdmissionTestRequest(t.TempDir(), "first", nil)
	request.Magnet.Name = " Movie: Part Two "
	entry := newTorrentQueueEntry(request, debridTypes.TorrentStatusQueued)
	if entry.OutputName == "" || entry.Name != request.Magnet.Name || entry.ContentPath != entry.DownloadPath() {
		t.Fatal("admission did not preserve title and assign output identity")
	}
	outputPath := entry.DownloadPath()
	applyDebridTorrentState(entry, &debridTypes.Torrent{Id: "provider-id", Debrid: "torbox", Name: "Provider: Renamed", InfoHash: entry.InfoHash})
	if entry.Name != "Provider: Renamed" || entry.DownloadPath() != outputPath {
		t.Fatal("provider result changed local output instead of only the display title")
	}
}

func TestTorrentOutputPathRestartKeepsPartAndExactCleanupTarget(t *testing.T) {
	root := t.TempDir()
	entry := torrentOwnershipTestEntry(root, "stable-restart", config.DownloadActionDownload)
	entry.Name = "Movie: Part Two"
	entry.OutputName = storage.NewTorrentOutputName(entry.Name, entry.InfoHash)
	if _, _, err := claimTorrentEntryDirectory(root, entry, torrentLegacyProof{}); err != nil {
		t.Fatal(err)
	}
	originalPath := entry.DownloadPath()
	part, err := openOwnedTorrentPart(root, entry, "movie.mkv", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.file.Write([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	if err := part.Close(); err != nil {
		t.Fatal(err)
	}
	store := newLifecycleTestStorage(t)
	if err := store.AddQueue(entry); err != nil {
		t.Fatal(err)
	}
	entry, err = store.GetQueued(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	entry.Name = "A new provider title"
	part, err = openOwnedTorrentPart(root, entry, "movie.mkv", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer part.Close()
	if size, err := part.Size(); err != nil || size != 2 {
		t.Fatalf("recovered partial size = %d, err=%v", size, err)
	}
	if _, err := part.file.WriteAt([]byte("cd"), 2); err != nil {
		t.Fatal(err)
	}
	if err := part.Commit(); err != nil {
		t.Fatal(err)
	}
	assertTorrentTestContents(t, filepath.Join(originalPath, "movie.mkv"), "abcd")
	foreign := *entry
	foreign.InfoHash = "different-owner"
	if err := removeOwnedTorrentEntryDirectory(root, &foreign); err == nil {
		t.Fatal("stored output name bypassed the ownership check")
	}
	assertTorrentTestContents(t, filepath.Join(originalPath, "movie.mkv"), "abcd")
	if err := removeOwnedTorrentEntryDirectory(root, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(originalPath); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove the original output: %v", err)
	}
}

func TestTorrentOutputPathSymlinkAndStrmUseStoredRoot(t *testing.T) {
	for _, action := range []config.DownloadAction{config.DownloadActionSymlink, config.DownloadActionStrm} {
		t.Run(string(action), func(t *testing.T) {
			if action == config.DownloadActionSymlink && runtime.GOOS == "windows" {
				t.Skip("symlink creation may require privileges")
			}
			root := t.TempDir()
			entry := torrentOwnershipTestEntry(root, "stable-links", action)
			entry.Name = "Movie: Part Two"
			entry.OutputName = storage.NewTorrentOutputName(entry.Name, entry.InfoHash)
			if _, _, err := claimTorrentEntryDirectory(root, entry, torrentLegacyProof{}); err != nil {
				t.Fatal(err)
			}
			mountFile := filepath.Join(t.TempDir(), "movie.mkv")
			if err := os.WriteFile(mountFile, []byte("media"), 0o600); err != nil {
				t.Fatal(err)
			}
			if action == config.DownloadActionSymlink {
				link, err := createOwnedTorrentSymlink(root, entry, "movie.mkv", mountFile, false)
				if err != nil {
					t.Fatal(err)
				}
				if link != filepath.Join(entry.DownloadPath(), "movie.mkv") {
					t.Fatal("symlink used a title-derived output root")
				}
				assertTorrentTestContents(t, link, "media")
			} else {
				if err := writeOwnedTorrentFile(root, entry, "movie.strm", []byte("https://example.invalid/stream\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				assertTorrentTestContents(t, filepath.Join(entry.DownloadPath(), "movie.strm"), "https://example.invalid/stream\n")
			}
			if err := removeOwnedTorrentEntryDirectory(root, entry); err != nil {
				t.Fatal(err)
			}
			assertTorrentTestContents(t, mountFile, "media")
		})
	}
}

func TestTorrentOutputPathRejectsUnsafePersistedComponents(t *testing.T) {
	root := t.TempDir()
	for _, component := range []string{"../escape", `/absolute`, `C:\escape`, "CON", "name:stream", "bad\x00name", "trailing ", torrentOwnerMarkerName, torrentPartialPrefix + "fake"} {
		entry := torrentOwnershipTestEntry(root, "unsafe-output", config.DownloadActionDownload)
		entry.OutputName = component
		if _, err := safeTorrentEntryDownloadPath(root, entry); err == nil {
			t.Errorf("accepted unsafe persisted component %q", component)
		}
	}
	entry := torrentOwnershipTestEntry(root, "unsafe-title", config.DownloadActionSymlink)
	entry.OutputName = storage.NewTorrentOutputName("safe", entry.InfoHash)
	entry.Name = "../../outside"
	if _, err := safeTorrentEntryDownloadPath(root, entry); err == nil {
		t.Fatal("safe output identity allowed an unsafe mount/source title")
	}
}

func TestTorrentOutputPathStripsOnlyRecognizedDisplayRoot(t *testing.T) {
	entry := torrentOwnershipTestEntry(t.TempDir(), "provider-root", config.DownloadActionSymlink)
	entry.Name = "Movie: Part Two"
	entry.OutputName = storage.NewTorrentOutputName(entry.Name, entry.InfoHash)
	for _, path := range []string{"Movie: Part Two/Season 01/movie.mkv", `Movie: Part Two\Season 01\movie.mkv`, " Movie: Part Two /Season 01/movie.mkv"} {
		entry.Files = map[string]*storage.File{"file": {Name: path, Path: path, Size: 4}}
		layouts, err := torrentEntryFileLayouts(entry)
		if err != nil || len(layouts) != 1 || layouts[0].relative != filepath.Join("Season 01", "movie.mkv") {
			t.Fatalf("display-root layout = %v, err=%v", layouts, err)
		}
	}
	for _, path := range []string{"Other: Root/movie.mkv", "Movie: Part Two/../escape.mkv", "Movie: Part Two/CON/movie.mkv", "Movie: Part Two/movie:stream.mkv", "/Movie: Part Two/movie.mkv"} {
		entry.Files = map[string]*storage.File{"file": {Name: "movie.mkv", Path: path, Size: 4}}
		if _, err := torrentEntryFileLayouts(entry); err == nil {
			t.Errorf("accepted unsafe nested path %q", path)
		}
	}
	entry.Name = ".."
	entry.Files = map[string]*storage.File{"file": {Name: "movie.mkv", Path: "../movie.mkv", Size: 4}}
	if _, err := torrentEntryFileLayouts(entry); err == nil {
		t.Fatal("untrusted entry title authorized stripping traversal")
	}
}

func TestTorrentOutputPathCollisionsAndSeasonSplits(t *testing.T) {
	root := t.TempDir()
	first := torrentOwnershipTestEntry(root, "first-torrent", config.DownloadActionDownload)
	first.Name = "Movie: Part Two"
	first.OutputName = storage.NewTorrentOutputName(first.Name, first.InfoHash)
	second := *first
	second.InfoHash = "second-torrent"
	second.Name = "Movie? Part Two"
	second.OutputName = storage.NewTorrentOutputName(second.Name, second.InfoHash)
	for _, entry := range []*storage.Entry{first, &second} {
		if _, _, err := claimTorrentEntryDirectory(root, entry, torrentLegacyProof{}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.EqualFold(first.DownloadPath(), second.DownloadPath()) {
		t.Fatal("sanitized titles collided")
	}
	seasons := []SeasonInfo{{InfoHash: "season-one", Name: "Series: S01"}, {InfoHash: "season-two", Name: "Series: S02"}}
	children := convertToMultiSeason(first, seasons)
	if children[0].OutputName == "" || children[0].OutputName == children[1].OutputName || children[0].OutputName == first.OutputName {
		t.Fatal("season split did not assign separate stable output identities")
	}
	first.OutputName = ""
	legacy := convertToMultiSeason(first, seasons)
	if legacy[0].OutputName != "" || legacy[1].OutputName != "" {
		t.Fatal("legacy season split silently opted into renamed output paths")
	}
}
