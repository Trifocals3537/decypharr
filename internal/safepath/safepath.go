// Package safepath provides filesystem boundaries for paths derived from
// configuration and remote/user-controlled identifiers.
package safepath

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

// ValidateRoot returns an absolute, cleaned root after rejecting locations
// that are too broad to be used as an application-owned data boundary.
// Existing symlinks anywhere in the path are rejected so a later child
// operation cannot be redirected outside the configured tree.
func ValidateRoot(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("path root is empty")
	}
	if strings.IndexByte(root, 0) >= 0 {
		return "", fmt.Errorf("path root contains a NUL byte")
	}
	if strings.IndexFunc(root, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("path root contains a control character")
	}

	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve path root: %w", err)
	}
	absolute = filepath.Clean(absolute)

	if isFilesystemRoot(absolute) {
		return "", fmt.Errorf("refusing filesystem root %q", absolute)
	}
	if filepath.Base(absolute) == "~" {
		return "", fmt.Errorf("refusing home-like path %q", absolute)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		homeAbsolute, absErr := filepath.Abs(home)
		if absErr == nil {
			homeAbsolute = filepath.Clean(homeAbsolute)
			if samePath(absolute, homeAbsolute) {
				return "", fmt.Errorf("refusing user home directory %q", absolute)
			}
			homeContainer := filepath.Dir(homeAbsolute)
			if !isFilesystemRoot(homeContainer) && samePath(absolute, homeContainer) {
				return "", fmt.Errorf("refusing home-directory container %q", absolute)
			}
		}
	}
	if err := RejectSymlinks(absolute); err != nil {
		return "", err
	}
	return absolute, nil
}

// JoinIdentifiers joins single-component identifiers under root. Identifiers
// may not be absolute paths, traversal components, or contain either platform's
// path separators. The returned path is absolute and symlink-checked.
func JoinIdentifiers(root string, identifiers ...string) (string, error) {
	absoluteRoot, err := ValidateRoot(root)
	if err != nil {
		return "", err
	}

	parts := make([]string, 0, len(identifiers)+1)
	parts = append(parts, absoluteRoot)
	for _, identifier := range identifiers {
		if err := ValidateIdentifier(identifier); err != nil {
			return "", err
		}
		parts = append(parts, identifier)
	}

	return ValidateUnderRoot(absoluteRoot, filepath.Join(parts...))
}

// ValidateIdentifier ensures value is a single, non-traversing filesystem
// component. Both slash styles are rejected so persisted data remains safe if
// it is moved between Linux and Windows.
func ValidateIdentifier(value string) error {
	if value == "" {
		return fmt.Errorf("path identifier is empty")
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("path identifier contains a NUL byte")
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("path identifier %q contains a control character", value)
	}
	if strings.ContainsRune(value, ':') {
		return fmt.Errorf("path identifier %q contains a Windows alternate-data-stream separator", value)
	}
	if strings.ContainsAny(value, `<>\"|?*`) {
		return fmt.Errorf("path identifier %q contains a non-portable Windows character", value)
	}
	if strings.HasSuffix(value, ".") || strings.HasSuffix(value, " ") {
		return fmt.Errorf("path identifier %q has a non-portable trailing dot or space", value)
	}
	if value == "." || value == ".." {
		return fmt.Errorf("path identifier %q is traversal", value)
	}
	if filepath.IsAbs(value) || filepath.VolumeName(value) != "" || looksLikeWindowsVolume(value) {
		return fmt.Errorf("path identifier %q is absolute", value)
	}
	if strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("path identifier %q contains a path separator", value)
	}
	if isWindowsReservedName(value) {
		return fmt.Errorf("path identifier %q is a reserved Windows device name", value)
	}
	return nil
}

// PortableNameKey returns the case-insensitive Windows-equivalent key for a
// validated identifier. It is suitable for collision detection before files
// are persisted on either Windows or a case-sensitive filesystem.
func PortableNameKey(value string) (string, error) {
	if err := ValidateIdentifier(value); err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimRight(value, " .")), nil
}

// ValidateUnderRoot proves target is a strict descendant of root and that no
// currently existing component in either path is a symlink.
func ValidateUnderRoot(root, target string) (string, error) {
	absoluteRoot, err := ValidateRoot(root)
	if err != nil {
		return "", err
	}
	if target == "" {
		return "", fmt.Errorf("target path is empty")
	}
	if strings.IndexByte(target, 0) >= 0 {
		return "", fmt.Errorf("target path contains a NUL byte")
	}
	if strings.IndexFunc(target, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("target path contains a control character")
	}

	absoluteTarget, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolve target path: %w", err)
	}
	absoluteTarget = filepath.Clean(absoluteTarget)

	relative, err := filepath.Rel(absoluteRoot, absoluteTarget)
	if err != nil {
		return "", fmt.Errorf("compare target with root: %w", err)
	}
	if relative == "." {
		return "", fmt.Errorf("target %q is the path root", absoluteTarget)
	}
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("target %q escapes root %q", absoluteTarget, absoluteRoot)
	}
	if err := RejectSymlinks(absoluteTarget); err != nil {
		return "", err
	}
	return absoluteTarget, nil
}

// EnsureDir creates target only after proving it is contained under root, then
// validates again so existing symlink components are never accepted.
func EnsureDir(root, target string, perm os.FileMode) (string, error) {
	absoluteTarget, err := ValidateUnderRoot(root, target)
	if err != nil {
		return "", err
	}
	absoluteRoot, err := ValidateRoot(root)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(absoluteRoot, perm); err != nil {
		return "", fmt.Errorf("create trusted root %q: %w", absoluteRoot, err)
	}
	rooted, relative, err := openRootTarget(absoluteRoot, absoluteTarget)
	if err != nil {
		return "", err
	}
	defer rooted.Close()
	if err := rooted.MkdirAll(relative, perm); err != nil {
		return "", fmt.Errorf("create directory %q beneath root: %w", relative, err)
	}
	return absoluteTarget, nil
}

// RemoveAll removes a strict descendant only after proving the complete
// target is contained. os.Root pins the trusted root and prevents a descendant
// symlink swap from redirecting deletion outside it.
func RemoveAll(root, target string) error {
	absoluteTarget, err := ValidateUnderRoot(root, target)
	if err != nil {
		return err
	}
	rooted, relative, err := openRootTarget(root, absoluteTarget)
	if err != nil {
		return err
	}
	defer rooted.Close()
	if err := rooted.RemoveAll(relative); err != nil {
		return fmt.Errorf("remove %q: %w", absoluteTarget, err)
	}
	return nil
}

// Remove removes one strict descendant through a pinned os.Root.
func Remove(root, target string) error {
	absoluteTarget, err := ValidateUnderRoot(root, target)
	if err != nil {
		return err
	}
	rooted, relative, err := openRootTarget(root, absoluteTarget)
	if err != nil {
		return err
	}
	defer rooted.Close()
	if err := rooted.Remove(relative); err != nil {
		return fmt.Errorf("remove %q: %w", absoluteTarget, err)
	}
	return nil
}

// OpenFile safely replaces a descendant file through a pinned os.Root.
// Existing targets are unlinked first and the replacement uses O_EXCL. This
// prevents both symlink redirection and truncation through an attacker-created
// hard link. Callers must request create-and-truncate semantics.
func OpenFile(root, target string, flag int, perm os.FileMode) (*os.File, error) {
	if flag&os.O_CREATE == 0 || flag&os.O_TRUNC == 0 {
		return nil, fmt.Errorf("safe OpenFile requires O_CREATE|O_TRUNC")
	}
	absoluteTarget, err := ValidateUnderRoot(root, target)
	if err != nil {
		return nil, err
	}
	rooted, relative, err := openRootTarget(root, absoluteTarget)
	if err != nil {
		return nil, err
	}

	if err := rooted.Remove(relative); err != nil && !os.IsNotExist(err) {
		_ = rooted.Close()
		return nil, fmt.Errorf("remove existing file %q: %w", absoluteTarget, err)
	}
	flag = (flag &^ os.O_TRUNC) | os.O_EXCL
	file, openErr := rooted.OpenFile(relative, flag, perm)
	closeErr := rooted.Close()
	if openErr != nil {
		return nil, fmt.Errorf("open file %q beneath root: %w", absoluteTarget, openErr)
	}
	if closeErr != nil {
		_ = file.Close()
		return nil, fmt.Errorf("close filesystem root: %w", closeErr)
	}
	return file, nil
}

// Symlink creates newPath through a pinned os.Root. The link target is
// deliberately not constrained to root: NZB mount files live outside the
// managed download tree, while the link itself must remain inside it. An
// existing link is accepted only when it already points to oldTarget; regular
// files and links to any other target are preserved and rejected.
func Symlink(root, oldTarget, newPath string) error {
	absoluteRoot, err := ValidateRoot(root)
	if err != nil {
		return err
	}
	if newPath == "" {
		return fmt.Errorf("symlink path is empty")
	}
	absoluteNewPath, err := filepath.Abs(newPath)
	if err != nil {
		return fmt.Errorf("resolve symlink path: %w", err)
	}
	absoluteNewPath = filepath.Clean(absoluteNewPath)
	leaf := filepath.Base(absoluteNewPath)
	if err := ValidateIdentifier(leaf); err != nil {
		return err
	}
	parent := filepath.Dir(absoluteNewPath)
	if samePath(parent, absoluteRoot) {
		if err := RejectSymlinks(parent); err != nil {
			return err
		}
	} else if _, err := ValidateUnderRoot(absoluteRoot, parent); err != nil {
		return err
	}

	rooted, relative, err := openRootTarget(absoluteRoot, absoluteNewPath)
	if err != nil {
		return err
	}
	defer rooted.Close()
	if err := rooted.Symlink(oldTarget, relative); err != nil {
		if os.IsExist(err) {
			info, statErr := rooted.Lstat(relative)
			if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
				existingTarget, readErr := rooted.Readlink(relative)
				if readErr == nil && existingTarget == oldTarget {
					return nil
				}
			}
		}
		return fmt.Errorf("create symlink %q -> %q beneath root: %w", absoluteNewPath, oldTarget, err)
	}
	return nil
}

func openRootTarget(root, absoluteTarget string) (*os.Root, string, error) {
	absoluteRoot, err := ValidateRoot(root)
	if err != nil {
		return nil, "", err
	}
	relative, err := filepath.Rel(absoluteRoot, absoluteTarget)
	if err != nil {
		return nil, "", fmt.Errorf("make target relative to root: %w", err)
	}
	if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("target %q escapes root %q", absoluteTarget, absoluteRoot)
	}
	rooted, err := os.OpenRoot(absoluteRoot)
	if err != nil {
		return nil, "", fmt.Errorf("open filesystem root %q: %w", absoluteRoot, err)
	}
	return rooted, relative, nil
}

// RejectSymlinks rejects any existing symlink component in path. It stops at
// the first missing component because no deeper component can exist yet.
func RejectSymlinks(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path for symlink check: %w", err)
	}
	absolute = filepath.Clean(absolute)

	volume := filepath.VolumeName(absolute)
	remainder := strings.TrimPrefix(absolute, volume)
	remainder = strings.TrimLeft(remainder, `/\`)

	current := volume + string(filepath.Separator)
	if volume == "" {
		current = string(filepath.Separator)
	}
	if remainder == "" {
		return nil
	}

	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("inspect path component %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %q is a symlink", current)
		}
	}
	return nil
}

func isFilesystemRoot(path string) bool {
	parent := filepath.Dir(path)
	return samePath(path, parent)
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func looksLikeWindowsVolume(value string) bool {
	return len(value) >= 2 &&
		((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) &&
		value[1] == ':'
}

func isWindowsReservedName(value string) bool {
	base := value
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	base = strings.ToUpper(strings.TrimRight(base, " ."))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$", "CONIN$", "CONOUT$":
		return true
	}
	runes := []rune(base)
	if len(runes) == 4 {
		prefix := string(runes[:3])
		number := runes[3]
		isDeviceDigit := (number >= '1' && number <= '9') ||
			number == '\u00b9' || number == '\u00b2' || number == '\u00b3'
		return (prefix == "COM" || prefix == "LPT") && isDeviceDigit
	}
	return false
}
