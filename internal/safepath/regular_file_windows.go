//go:build windows

package safepath

import "os"

func replaceRootFile(rooted *os.Root, source, destination string) error {
	// os.Root.Rename uses directory handles plus replace-if-exists semantics on
	// Windows, so a renamed or swapped parent path cannot redirect replacement.
	return rooted.Rename(source, destination)
}

func syncOpenRoot(*os.Root) error {
	// Windows has no portable directory-fsync operation. The temporary file is
	// synced before the atomic rooted rename, and recovery files remain valid if
	// the namespace update is interrupted.
	return nil
}
