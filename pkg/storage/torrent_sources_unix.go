//go:build !windows

package storage

import "os"

func replaceTorrentSource(source, destination string) error {
	return os.Rename(source, destination)
}

func syncTorrentSourceDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
