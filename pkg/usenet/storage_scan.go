package usenet

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// scanMetadataDirectory walks a directory with bounded memory. os.ReadDir
// returns the entire directory in one allocation, which becomes expensive for
// long-lived installations with many NZBs.
func scanMetadataDirectory(
	path string,
	batchSize int,
	fn func(os.DirEntry) error,
) (returnErr error) {
	if batchSize <= 0 {
		return fmt.Errorf("metadata directory batch size must be positive")
	}
	if fn == nil {
		return fmt.Errorf("metadata directory callback is nil")
	}

	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := directory.Close(); closeErr != nil && returnErr == nil {
			returnErr = fmt.Errorf("close metadata directory: %w", closeErr)
		}
	}()

	for {
		entries, readErr := directory.ReadDir(batchSize)
		for _, entry := range entries {
			if err := fn(entry); err != nil {
				return err
			}
		}
		switch {
		case readErr == nil:
		case errors.Is(readErr, io.EOF):
			return nil
		default:
			return readErr
		}
	}
}
