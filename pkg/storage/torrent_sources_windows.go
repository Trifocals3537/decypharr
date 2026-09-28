//go:build windows

package storage

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func replaceTorrentSource(rooted *os.Root, source, destination string) error {
	sourcePtr, err := windows.UTF16PtrFromString(filepath.Join(rooted.Name(), source))
	if err != nil {
		return err
	}
	destinationPtr, err := windows.UTF16PtrFromString(filepath.Join(rooted.Name(), destination))
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		sourcePtr,
		destinationPtr,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

func syncTorrentSourceDirectory(*os.Root) error {
	// MoveFileEx with WRITE_THROUGH flushes the replacement on Windows.
	return nil
}
