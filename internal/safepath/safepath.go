// Package safepath provides filesystem boundaries for paths derived from
// configuration and remote/user-controlled identifiers.
package safepath

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// PortableIdentifierMaxBytes is a conservative component limit that is
	// accepted by the filesystems Decypharr supports. Counting UTF-8 bytes is
	// also conservative for Windows UTF-16 component limits.
	PortableIdentifierMaxBytes      = 255
	compactIdentifierDigestHexBytes = sha256.Size * 2
	compactIdentifierMaxExt         = 16
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

// OpenRoot validates and pins an existing application-owned filesystem root.
// The returned absolute path is the same boundary represented by rooted.
func OpenRoot(root string) (*os.Root, string, error) {
	absolute, err := ValidateRoot(root)
	if err != nil {
		return nil, "", err
	}
	rooted, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, "", fmt.Errorf("open filesystem root %q: %w", absolute, err)
	}
	if err := verifyPinnedRoot(rooted, absolute); err != nil {
		_ = rooted.Close()
		return nil, "", err
	}
	return rooted, absolute, nil
}

func verifyPinnedRoot(rooted *os.Root, visiblePath string) error {
	openedInfo, err := rooted.Stat(".")
	if err != nil {
		return fmt.Errorf("inspect pinned filesystem root %q: %w", visiblePath, err)
	}
	visibleInfo, err := os.Lstat(visiblePath)
	if err != nil {
		return fmt.Errorf("reinspect filesystem root %q: %w", visiblePath, err)
	}
	if visibleInfo.Mode()&os.ModeSymlink != 0 || !visibleInfo.IsDir() || !os.SameFile(openedInfo, visibleInfo) {
		return fmt.Errorf("filesystem root %q changed during validation", visiblePath)
	}
	return nil
}

// EnsureRoot creates an application-owned filesystem root without passing the
// complete untrusted path to a recursive creation primitive. It validates all
// existing components, pins the nearest existing ancestor with os.Root, and
// creates only a local relative descendant beneath that pinned directory.
func EnsureRoot(root string, perm os.FileMode) (string, error) {
	absolute, err := ValidateRoot(root)
	if err != nil {
		return "", err
	}

	ancestor := absolute
	var rooted *os.Root
	for {
		rooted, err = os.OpenRoot(ancestor)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("open filesystem ancestor %q: %w", ancestor, err)
		}
		parent := filepath.Dir(ancestor)
		if samePath(parent, ancestor) {
			return "", fmt.Errorf("no existing ancestor for filesystem root %q", absolute)
		}
		ancestor = parent
	}
	defer rooted.Close()

	// ValidateRoot checked this path before it was opened. Comparing the pinned
	// handle with the visible path closes the symlink-swap window around open.
	if err := verifyPinnedRoot(rooted, ancestor); err != nil {
		return "", err
	}

	relative, err := filepath.Rel(ancestor, absolute)
	if err != nil {
		return "", fmt.Errorf("make filesystem root relative to ancestor: %w", err)
	}
	if relative != "." {
		if !filepath.IsLocal(relative) {
			return "", fmt.Errorf("filesystem root %q escapes ancestor %q", absolute, ancestor)
		}
		if err := rooted.MkdirAll(relative, perm.Perm()); err != nil {
			return "", fmt.Errorf("create filesystem root %q: %w", absolute, err)
		}
		created, err := rooted.OpenRoot(relative)
		if err != nil {
			return "", fmt.Errorf("pin created filesystem root %q: %w", absolute, err)
		}
		if err := verifyPinnedRoot(created, absolute); err != nil {
			_ = created.Close()
			return "", err
		}
		if err := created.Close(); err != nil {
			return "", fmt.Errorf("close created filesystem root %q: %w", absolute, err)
		}
	}

	validated, err := ValidateRoot(absolute)
	if err != nil {
		return "", fmt.Errorf("revalidate created filesystem root: %w", err)
	}
	return validated, nil
}

// JoinIdentifiers joins single-component identifiers under root. Identifiers
// may not be absolute paths, traversal components, or contain either platform's
// path separators. The returned path is absolute and symlink-checked.
func JoinIdentifiers(root string, identifiers ...string) (string, error) {
	absoluteRoot, err := ValidateRoot(root)
	if err != nil {
		return "", err
	}

	parts := []string{absoluteRoot}
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

// CompactIdentifier returns a deterministic, portable filesystem component.
// The original value is validated before any shortening so traversal,
// separators, control characters, reserved device names, and other unsafe
// input cannot be hidden beyond the retained prefix. Values already within the
// limit are returned unchanged. Oversized values retain a readable UTF-8
// prefix and short extension plus the full SHA-256 digest of the original.
func CompactIdentifier(value string, maxBytes int) (string, error) {
	if err := ValidateIdentifier(value); err != nil {
		return "", err
	}
	if maxBytes <= 0 {
		return "", fmt.Errorf("identifier byte limit must be positive")
	}
	if len(value) <= maxBytes {
		return value, nil
	}

	extension := filepath.Ext(value)
	if len(extension) > compactIdentifierMaxExt {
		extension = ""
	}
	digest := sha256.Sum256([]byte(value))
	suffix := "~" + hex.EncodeToString(digest[:])
	prefixBudget := maxBytes - 1 - compactIdentifierDigestHexBytes - len(extension)
	if prefixBudget < 1 {
		return "", fmt.Errorf(
			"identifier byte limit %d is too small for collision-safe compaction",
			maxBytes,
		)
	}

	prefix := value[:utf8PrefixBytes(value, prefixBudget)]
	prefix = strings.TrimRight(prefix, " .")
	if prefix == "" {
		prefix = "item"
		if len(prefix) > prefixBudget {
			return "", fmt.Errorf(
				"identifier byte limit %d is too small for a portable compacted prefix",
				maxBytes,
			)
		}
	}
	compacted := prefix + suffix + extension
	if len(compacted) > maxBytes {
		return "", fmt.Errorf("compacted identifier exceeds %d-byte limit", maxBytes)
	}
	if err := ValidateIdentifier(compacted); err != nil {
		return "", fmt.Errorf("compacted identifier is invalid: %w", err)
	}
	return compacted, nil
}

func utf8PrefixBytes(value string, maxBytes int) int {
	if maxBytes <= 0 {
		return 0
	}
	if len(value) <= maxBytes {
		return len(value)
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return end
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
	absoluteRoot, err := EnsureRoot(root, perm)
	if err != nil {
		return "", err
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

// Rename atomically replaces one strict descendant with another through a
// pinned os.Root. Both paths must remain beneath the same trusted root.
func Rename(root, oldPath, newPath string) error {
	absoluteOld, err := ValidateUnderRoot(root, oldPath)
	if err != nil {
		return err
	}
	absoluteNew, err := ValidateUnderRoot(root, newPath)
	if err != nil {
		return err
	}
	rooted, oldRelative, err := openRootTarget(root, absoluteOld)
	if err != nil {
		return err
	}
	defer rooted.Close()
	newRoot, newRelative, err := openRootTarget(root, absoluteNew)
	if err != nil {
		return err
	}
	if err := newRoot.Close(); err != nil {
		return fmt.Errorf("close filesystem root: %w", err)
	}
	if err := rooted.Rename(oldRelative, newRelative); err != nil {
		return fmt.Errorf("rename %q to %q: %w", absoluteOld, absoluteNew, err)
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
				if readErr == nil && sameSymlinkDestination(absoluteNewPath, existingTarget, oldTarget) {
					return nil
				}
			}
		}
		return fmt.Errorf("create symlink %q -> %q beneath root: %w", absoluteNewPath, oldTarget, err)
	}
	return nil
}

func sameSymlinkDestination(linkPath, left, right string) bool {
	resolve := func(target string) (string, error) {
		if target == "" {
			return "", fmt.Errorf("symlink target is empty")
		}
		if filepath.IsAbs(target) {
			return filepath.Clean(target), nil
		}
		return filepath.Clean(filepath.Join(filepath.Dir(linkPath), target)), nil
	}
	resolvedLeft, leftErr := resolve(left)
	resolvedRight, rightErr := resolve(right)
	return leftErr == nil && rightErr == nil && samePath(resolvedLeft, resolvedRight)
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
