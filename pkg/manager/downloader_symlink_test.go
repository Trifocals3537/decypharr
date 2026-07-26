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
