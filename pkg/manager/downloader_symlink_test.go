package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestCreateTorrentSymlinksKeepsExistingDestinationCompatibility(t *testing.T) {
	config.SetConfigPath(t.TempDir())
	downloadRoot := t.TempDir()
	mountPath := t.TempDir()
	fileName := "movie.mkv"
	target := filepath.Join(mountPath, fileName)
	if err := os.WriteFile(target, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := &storage.Entry{
		InfoHash: "same-target-symlink",
		Name:     "release",
		Protocol: config.ProtocolTorrent,
		SavePath: downloadRoot,
		Files: map[string]*storage.File{
			fileName: {Name: fileName},
		},
	}
	symlinkDir, _, err := claimTorrentEntryDirectory(downloadRoot, entry, torrentLegacyProof{})
	if err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(symlinkDir, fileName)
	if err := os.Symlink(target, existing); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	downloader := &Downloader{dest: downloadRoot, logger: zerolog.Nop()}
	files := []*storage.File{{Name: fileName}}
	paths, err := downloader.createSymlinksWhenMountFilesAppear(context.Background(), entry, files, mountPath, symlinkDir)
	if err != nil {
		t.Fatalf("existing torrent symlink should remain compatible: %v", err)
	}
	if len(paths) != 1 || paths[0] != existing {
		t.Fatalf("created paths = %v, want [%s]", paths, existing)
	}
}

func TestCreateUsenetSymlinksSkipsMatchingDirectoryName(t *testing.T) {
	downloadRoot := t.TempDir()
	mountPath := t.TempDir()
	entry := normalNZBEntry(downloadRoot)
	fileName := "Show S01E01.mkv"

	nestedDirectory := filepath.Join(mountPath, fileName)
	if err := os.Mkdir(nestedDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	wantTarget := filepath.Join(nestedDirectory, fileName)
	if err := os.WriteFile(wantTarget, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	probeLink := filepath.Join(mountPath, "symlink-capability-probe")
	if err := os.Symlink(wantTarget, probeLink); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := os.Remove(probeLink); err != nil {
		t.Fatalf("remove symlink capability probe: %v", err)
	}

	symlinkDir, _, err := claimUsenetEntryDirectory(downloadRoot, entry)
	if err != nil {
		t.Fatal(err)
	}
	downloader := &Downloader{dest: downloadRoot, logger: zerolog.Nop()}
	paths, err := downloader.createSymlinksWhenMountFilesAppear(
		context.Background(),
		entry,
		[]*storage.File{{Name: fileName}},
		mountPath,
		symlinkDir,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("created %d symlinks, want 1", len(paths))
	}
	gotTarget, err := os.Readlink(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if gotTarget != wantTarget {
		t.Fatalf("symlink target = %q, want nested file %q", gotTarget, wantTarget)
	}
	info, err := os.Stat(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		t.Fatal("symlink target is a directory, want a regular file")
	}
}
