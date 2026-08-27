package storage

import (
	"bytes"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/testutil"
	"github.com/sirrobot01/decypharr/internal/utils"
)

func testTorrentSource(t *testing.T) ([]byte, *utils.Magnet) {
	t.Helper()
	data, err := os.ReadFile(testutil.GetTestTorrentPath())
	if err != nil {
		t.Fatal(err)
	}
	magnet, err := utils.GetMagnetFromBytes(data, false)
	if err != nil {
		t.Fatal(err)
	}
	return data, magnet
}

func TestTorrentSourceRoundTripSurvivesStorageRestart(t *testing.T) {
	data, magnet := testTorrentSource(t)
	dbPath := t.TempDir()
	store, err := NewStorage(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTorrentSource(strings.ToUpper(magnet.InfoHash), data); err != nil {
		t.Fatalf("SaveTorrentSource() error = %v", err)
	}
	if err := store.AddQueue(&Entry{InfoHash: magnet.InfoHash, Name: magnet.Name}); err != nil {
		t.Fatalf("AddQueue() error = %v", err)
	}
	path, err := store.torrentSourcePath(magnet.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != torrentSourceMode {
			t.Fatalf("torrent source mode = %o, want %o", got, torrentSourceMode)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewStorage(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.LoadTorrentSource(magnet.InfoHash)
	if err != nil {
		t.Fatalf("LoadTorrentSource() error = %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("reloaded torrent source differs from admitted bytes")
	}
}

func TestTorrentSourceRejectsMismatchesAndCorruption(t *testing.T) {
	data, magnet := testTorrentSource(t)
	store, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.SaveTorrentSource("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", data); err == nil {
		t.Fatal("SaveTorrentSource() accepted mismatched content")
	}
	if err := store.SaveTorrentSource("../"+magnet.InfoHash, data); err == nil {
		t.Fatal("SaveTorrentSource() accepted an unsafe key")
	}
	if err := store.SaveTorrentSource(magnet.InfoHash, data); err != nil {
		t.Fatal(err)
	}
	path, err := store.torrentSourcePath(magnet.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a torrent"), torrentSourceMode); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadTorrentSource(magnet.InfoHash); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadTorrentSource() error = %v, want a corruption error", err)
	}
}

func TestTorrentSourceStartupPrunesOrphans(t *testing.T) {
	data, magnet := testTorrentSource(t)
	dbPath := t.TempDir()
	store, err := NewStorage(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTorrentSource(magnet.InfoHash, data); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewStorage(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.LoadTorrentSource(magnet.InfoHash); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned LoadTorrentSource() error = %v, want not found", err)
	}
}

func TestTorrentSourceStoreEnforcesTotalQuota(t *testing.T) {
	data, magnet := testTorrentSource(t)
	store, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	previousLimit := torrentSourceStoreMaxBytes
	torrentSourceStoreMaxBytes = int64(len(data) - 1)
	defer func() { torrentSourceStoreMaxBytes = previousLimit }()
	if err := store.SaveTorrentSource(magnet.InfoHash, data); err == nil {
		t.Fatal("SaveTorrentSource() accepted data beyond the total quota")
	}
}
