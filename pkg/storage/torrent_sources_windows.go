//go:build windows

package storage

import "golang.org/x/sys/windows"

func replaceTorrentSource(source, destination string) error {
	sourcePtr, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPtr, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		sourcePtr,
		destinationPtr,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

func syncTorrentSourceDirectory(string) error {
	// MoveFileEx with WRITE_THROUGH flushes the replacement on Windows.
	return nil
}
