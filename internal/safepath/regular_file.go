package safepath

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// OpenRegularFile opens one absolute regular-file path through a pinned parent
// directory. The final component must be a portable identifier, symlinks and
// special files are rejected, and the opened handle must still identify the
// directory entry inspected before the open.
func OpenRegularFile(path string, flag int, perm os.FileMode) (*os.File, bool, error) {
	rooted, leaf, absolute, err := openRegularFileParent(path)
	if err != nil {
		return nil, false, err
	}
	defer rooted.Close()

	for attempt := 0; attempt < 2; attempt++ {
		before, statErr := rooted.Lstat(leaf)
		switch {
		case statErr == nil:
			if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
				return nil, false, fmt.Errorf("file path is not a regular non-symlink file: %s", absolute)
			}
			if flag&os.O_EXCL != 0 && flag&os.O_CREATE != 0 {
				return nil, false, &os.PathError{Op: "open", Path: absolute, Err: os.ErrExist}
			}
			file, openErr := rooted.OpenFile(leaf, flag&^os.O_CREATE, perm)
			if openErr != nil {
				return nil, false, openErr
			}
			opened, openedErr := file.Stat()
			after, afterErr := rooted.Lstat(leaf)
			if openedErr == nil && afterErr == nil && after.Mode()&os.ModeSymlink == 0 &&
				after.Mode().IsRegular() && os.SameFile(before, opened) && os.SameFile(opened, after) {
				return file, false, nil
			}
			closeErr := file.Close()
			return nil, false, errors.Join(
				openedErr,
				afterErr,
				closeErr,
				fmt.Errorf("file path changed while opening: %s", absolute),
			)
		case !isNotExistError(statErr):
			return nil, false, statErr
		case flag&os.O_CREATE == 0:
			return nil, false, &os.PathError{Op: "open", Path: absolute, Err: os.ErrNotExist}
		}

		file, createErr := rooted.OpenFile(leaf, flag|os.O_EXCL, perm)
		if createErr == nil {
			info, infoErr := file.Stat()
			if infoErr == nil && info.Mode().IsRegular() {
				return file, true, nil
			}
			closeErr := file.Close()
			removeErr := rooted.Remove(leaf)
			return nil, false, errors.Join(
				infoErr,
				closeErr,
				removeErr,
				fmt.Errorf("created path is not a regular file: %s", absolute),
			)
		}
		if !errors.Is(createErr, os.ErrExist) {
			return nil, false, createErr
		}
	}
	return nil, false, fmt.Errorf("file path changed repeatedly while opening: %s", absolute)
}

// ReadRegularFile reads a bounded regular file without following a symlink.
func ReadRegularFile(path string, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, fmt.Errorf("regular file read limit must not be negative")
	}
	file, _, err := OpenRegularFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := io.Reader(file)
	if limit > 0 {
		reader = io.LimitReader(file, limit+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, fmt.Errorf("regular file exceeds %d-byte limit", limit)
	}
	return data, nil
}

// ChmodRegularFile changes permissions through an already validated file
// handle rather than performing a second pathname traversal.
func ChmodRegularFile(path string, mode os.FileMode) error {
	file, _, err := OpenRegularFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	return errors.Join(file.Chmod(mode), file.Close())
}

// StatRegularFile returns metadata from an opened regular-file handle.
func StatRegularFile(path string) (os.FileInfo, error) {
	file, _, err := OpenRegularFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	info, statErr := file.Stat()
	return info, errors.Join(statErr, file.Close())
}

// RemoveRegularFile removes one regular file through its pinned parent.
func RemoveRegularFile(path string) error {
	rooted, leaf, absolute, err := openRegularFileParent(path)
	if err != nil {
		return err
	}
	defer rooted.Close()
	info, err := rooted.Lstat(leaf)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("file path is not a regular non-symlink file: %s", absolute)
	}
	return rooted.Remove(leaf)
}

// RenameRegularFile renames a regular file within one pinned parent
// directory. Existing destinations must also be regular non-symlink files.
func RenameRegularFile(source, destination string) error {
	sourceRoot, sourceLeaf, sourceAbsolute, err := openRegularFileParent(source)
	if err != nil {
		return err
	}
	defer sourceRoot.Close()
	destinationAbsolute, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("resolve destination file path: %w", err)
	}
	destinationAbsolute = filepath.Clean(destinationAbsolute)
	if !samePath(filepath.Dir(sourceAbsolute), filepath.Dir(destinationAbsolute)) {
		return fmt.Errorf("regular-file rename crosses parent directories")
	}
	destinationLeaf := filepath.Base(destinationAbsolute)
	if err := ValidateIdentifier(destinationLeaf); err != nil {
		return fmt.Errorf("invalid destination file name: %w", err)
	}
	sourceInfo, err := sourceRoot.Lstat(sourceLeaf)
	if err != nil {
		return err
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("source path is not a regular non-symlink file: %s", sourceAbsolute)
	}
	if destinationInfo, destinationErr := sourceRoot.Lstat(destinationLeaf); destinationErr == nil {
		if destinationInfo.Mode()&os.ModeSymlink != 0 || !destinationInfo.Mode().IsRegular() {
			return fmt.Errorf("destination path is not a regular non-symlink file: %s", destinationAbsolute)
		}
	} else if !isNotExistError(destinationErr) {
		return destinationErr
	}
	return sourceRoot.Rename(sourceLeaf, destinationLeaf)
}

// AtomicWriteFile crash-safely replaces one regular file through a pinned
// parent. A random sibling is fully written and synced before the final rename.
func AtomicWriteFile(path string, data []byte, mode os.FileMode) (returnErr error) {
	rooted, leaf, _, err := openRegularFileParent(path)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, rooted.Close()) }()

	temporary, err := randomSiblingName()
	if err != nil {
		return err
	}
	file, err := rooted.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	removeTemporary := true
	fileClosed := false
	defer func() {
		var closeErr error
		if !fileClosed {
			closeErr = file.Close()
		}
		if removeTemporary {
			removeErr := rooted.Remove(temporary)
			if errors.Is(removeErr, os.ErrNotExist) {
				removeErr = nil
			}
			returnErr = errors.Join(returnErr, closeErr, removeErr)
		}
	}()

	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	fileClosed = true
	if err := replaceRootFile(rooted, temporary, leaf); err != nil {
		return err
	}
	removeTemporary = false
	return syncOpenRoot(rooted)
}

// SyncDirectory syncs an existing directory through a pinned root.
func SyncDirectory(path string) error {
	rooted, _, err := OpenRoot(path)
	if err != nil {
		return err
	}
	defer rooted.Close()
	return syncOpenRoot(rooted)
}

func openRegularFileParent(path string) (*os.Root, string, string, error) {
	if path == "" {
		return nil, "", "", fmt.Errorf("file path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", "", fmt.Errorf("resolve file path: %w", err)
	}
	absolute = filepath.Clean(absolute)
	leaf := filepath.Base(absolute)
	if err := ValidateIdentifier(leaf); err != nil {
		return nil, "", "", fmt.Errorf("invalid file name: %w", err)
	}
	rooted, _, err := OpenRoot(filepath.Dir(absolute))
	if err != nil {
		if isNotExistError(err) {
			return nil, "", "", &os.PathError{Op: "open", Path: absolute, Err: os.ErrNotExist}
		}
		return nil, "", "", err
	}
	return rooted, leaf, absolute, nil
}

func randomSiblingName() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate atomic-write file name: %w", err)
	}
	return ".tessarr-write-" + hex.EncodeToString(entropy[:]) + ".tmp", nil
}
