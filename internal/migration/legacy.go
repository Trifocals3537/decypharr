// Package migration contains offline, one-way state migration helpers.
package migration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sirrobot01/decypharr/internal/safepath"
)

const (
	receiptName    = ".tessarr-migration.json"
	receiptVersion = 1
)

var exactStateNames = map[string]string{
	".decypharr":                             ".tessarr",
	".decypharr-cache-cleanup.lock":          ".tessarr-cache-cleanup.lock",
	".decypharr-cache-instance":              ".tessarr-cache-instance",
	".decypharr-cache-owner":                 ".tessarr-cache-owner",
	".decypharr-nzb-legacy-adoption-v1.done": ".tessarr-nzb-legacy-adoption-v1.done",
	".decypharr-nzb-owner-v1":                ".tessarr-nzb-owner-v1",
	".decypharr-nzb-ownership.lock":          ".tessarr-nzb-ownership.lock",
	".decypharr-stream-cache-v1":             ".tessarr-stream-cache-v1",
	".decypharr-torrent-owner-v1":            ".tessarr-torrent-owner-v1",
	".decypharr-torrent-ownership.lock":      ".tessarr-torrent-ownership.lock",
	"decypharr.log":                          "tessarr.log",
}

var prefixedStateNames = []struct {
	old string
	new string
}{
	{old: ".decypharr-cache-quarantine-", new: ".tessarr-cache-quarantine-"},
	{old: ".decypharr-nzb-quarantine-", new: ".tessarr-nzb-quarantine-"},
	{old: ".decypharr-torrent-part-", new: ".tessarr-torrent-part-"},
	{old: ".decypharr-torrent-quarantine-", new: ".tessarr-torrent-quarantine-"},
}

// Options controls a Decypharr-to-Tessarr state migration.
type Options struct {
	Source string
	Target string
	DryRun bool
}

// Result summarizes a completed or planned migration.
type Result struct {
	Files           int
	Directories     int
	Bytes           int64
	RenamedEntries  int
	AlreadyMigrated bool
	DryRun          bool
}

type manifestEntry struct {
	SourcePath string
	TargetPath string
	Mode       fs.FileMode
	Size       int64
	ModTime    time.Time
	Digest     string
}

type receipt struct {
	SchemaVersion int    `json:"schema_version"`
	SourceDigest  string `json:"source_digest"`
}

// Migrate copies an offline Decypharr state tree into a new Tessarr state
// tree. The source is never mutated, symlinks and special files are rejected,
// and the fully verified staging tree is promoted with one rename.
func Migrate(options Options) (Result, error) {
	var result Result
	result.DryRun = options.DryRun

	source, target, err := validateRoots(options.Source, options.Target)
	if err != nil {
		return result, err
	}

	entries, err := buildManifest(source)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if entry.Mode.IsDir() {
			result.Directories++
			continue
		}
		result.Files++
		result.Bytes += entry.Size
		if entry.SourcePath != entry.TargetPath {
			result.RenamedEntries++
		}
	}
	sourceDigest := digestManifest(entries)

	if _, err := os.Lstat(target); err == nil {
		if err := verifyExistingTarget(target, entries, sourceDigest); err != nil {
			return result, err
		}
		result.AlreadyMigrated = true
		return result, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, fmt.Errorf("inspect migration target: %w", err)
	}
	if options.DryRun {
		return result, nil
	}

	parent := filepath.Dir(target)
	stage, err := os.MkdirTemp(parent, ".tessarr-migration-")
	if err != nil {
		return result, fmt.Errorf("create migration staging directory: %w", err)
	}
	promoted := false
	defer func() {
		if !promoted {
			_ = os.RemoveAll(stage)
		}
	}()

	if err := copyManifest(source, stage, entries); err != nil {
		return result, err
	}
	if err := verifyTree(stage, entries); err != nil {
		return result, fmt.Errorf("verify staged Tessarr state: %w", err)
	}

	currentEntries, err := buildManifest(source)
	if err != nil {
		return result, fmt.Errorf("reinspect source after copy: %w", err)
	}
	if digestManifest(currentEntries) != sourceDigest {
		return result, fmt.Errorf("source state changed during migration; stop the service and retry")
	}

	if err := writeReceipt(stage, sourceDigest); err != nil {
		return result, err
	}
	if err := os.Rename(stage, target); err != nil {
		return result, fmt.Errorf("promote verified Tessarr state: %w", err)
	}
	promoted = true
	return result, nil
}

func validateRoots(source, target string) (string, string, error) {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(target) == "" {
		return "", "", fmt.Errorf("source and target are required")
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return "", "", fmt.Errorf("resolve migration source: %w", err)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", "", fmt.Errorf("resolve migration target: %w", err)
	}
	source = filepath.Clean(source)
	target = filepath.Clean(target)
	if samePath(source, target) {
		return "", "", fmt.Errorf("source and target must be different directories")
	}
	if pathContains(source, target) || pathContains(target, source) {
		return "", "", fmt.Errorf("source and target may not contain one another")
	}
	info, err := os.Lstat(source)
	if err != nil {
		return "", "", fmt.Errorf("inspect migration source: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", "", fmt.Errorf("migration source must be a real directory")
	}
	if err := safepath.RejectSymlinks(source); err != nil {
		return "", "", fmt.Errorf("validate migration source: %w", err)
	}
	targetParent := filepath.Dir(target)
	if err := safepath.RejectSymlinks(targetParent); err != nil {
		return "", "", fmt.Errorf("validate migration target parent: %w", err)
	}
	parentInfo, err := os.Lstat(targetParent)
	if err != nil {
		return "", "", fmt.Errorf("inspect migration target parent: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return "", "", fmt.Errorf("migration target parent must be a real directory")
	}
	return source, target, nil
}

func buildManifest(source string) ([]manifestEntry, error) {
	entries := make([]manifestEntry, 0)
	portablePaths := make(map[string]string)
	err := filepath.WalkDir(source, func(path string, dirEntry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if samePath(path, source) {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in migration source: %s", path)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("refusing special file in migration source: %s", path)
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || !filepath.IsLocal(relative) {
			return fmt.Errorf("source entry escapes migration root: %s", path)
		}
		targetPath := transformRelativePath(relative)
		key := portablePathKey(targetPath)
		if previous, exists := portablePaths[key]; exists {
			return fmt.Errorf("migration name collision: %q and %q both map to %q", previous, relative, targetPath)
		}
		portablePaths[key] = relative

		entry := manifestEntry{
			SourcePath: relative,
			TargetPath: targetPath,
			Mode:       info.Mode(),
			Size:       info.Size(),
			ModTime:    info.ModTime(),
		}
		if info.Mode().IsRegular() {
			entry.Digest, err = digestFile(path)
			if err != nil {
				return err
			}
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inspect migration source: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].TargetPath < entries[j].TargetPath
	})
	return entries, nil
}

func transformRelativePath(relative string) string {
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	for index, part := range parts {
		parts[index] = transformStateName(part)
	}
	return filepath.Join(parts...)
}

func transformStateName(name string) string {
	if replacement, exists := exactStateNames[name]; exists {
		return replacement
	}
	for _, replacement := range prefixedStateNames {
		if strings.HasPrefix(name, replacement.old) {
			return replacement.new + strings.TrimPrefix(name, replacement.old)
		}
	}
	return name
}

func portablePathKey(path string) string {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for index, part := range parts {
		parts[index] = strings.ToLower(strings.TrimRight(part, " ."))
	}
	return strings.Join(parts, "/")
}

func copyManifest(source, stage string, entries []manifestEntry) error {
	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.SourcePath)
		targetPath := filepath.Join(stage, entry.TargetPath)
		if entry.Mode.IsDir() {
			if err := os.Mkdir(targetPath, entry.Mode.Perm()); err != nil {
				return fmt.Errorf("create staged directory %q: %w", entry.TargetPath, err)
			}
			continue
		}
		if err := copyFile(sourcePath, targetPath, entry.Mode.Perm()); err != nil {
			return fmt.Errorf("copy state file %q: %w", entry.SourcePath, err)
		}
		if err := os.Chtimes(targetPath, entry.ModTime, entry.ModTime); err != nil {
			return fmt.Errorf("preserve timestamp for %q: %w", entry.TargetPath, err)
		}
	}
	return nil
}

func copyFile(source, target string, mode fs.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	copyErr := error(nil)
	if _, err := io.Copy(output, input); err != nil {
		copyErr = err
	} else if err := output.Sync(); err != nil {
		copyErr = err
	}
	if err := output.Close(); copyErr == nil {
		copyErr = err
	}
	return copyErr
}

func verifyTree(root string, expected []manifestEntry) error {
	actual, err := scanTarget(root)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("entry count is %d, expected %d", len(actual), len(expected))
	}
	for index := range expected {
		want := expected[index]
		got := actual[index]
		if want.TargetPath != got.TargetPath || want.Mode.Type() != got.Mode.Type() ||
			want.Mode.Perm() != got.Mode.Perm() || want.Size != got.Size || want.Digest != got.Digest {
			return fmt.Errorf("staged entry %q does not match source", want.TargetPath)
		}
	}
	return nil
}

func scanTarget(root string) ([]manifestEntry, error) {
	entries := make([]manifestEntry, 0)
	err := filepath.WalkDir(root, func(path string, dirEntry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if samePath(path, root) {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || !filepath.IsLocal(relative) {
			return fmt.Errorf("target entry escapes migration root: %s", path)
		}
		if relative == receiptName {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("unexpected target entry type: %s", path)
		}
		entry := manifestEntry{TargetPath: relative, Mode: info.Mode(), Size: info.Size()}
		if info.Mode().IsRegular() {
			entry.Digest, err = digestFile(path)
			if err != nil {
				return err
			}
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].TargetPath < entries[j].TargetPath
	})
	return entries, nil
}

func writeReceipt(stage, sourceDigest string) error {
	data, err := json.MarshalIndent(receipt{SchemaVersion: receiptVersion, SourceDigest: sourceDigest}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode migration receipt: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(stage, receiptName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write migration receipt: %w", err)
	}
	return nil
}

func verifyExistingTarget(target string, expected []manifestEntry, sourceDigest string) error {
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("migration target exists and is not a real directory")
	}
	data, err := os.ReadFile(filepath.Join(target, receiptName))
	if err != nil {
		return fmt.Errorf("migration target already exists without a valid Tessarr receipt: %w", err)
	}
	var completed receipt
	if err := json.Unmarshal(data, &completed); err != nil {
		return fmt.Errorf("decode migration receipt: %w", err)
	}
	if completed.SchemaVersion != receiptVersion || completed.SourceDigest != sourceDigest {
		return fmt.Errorf("migration target belongs to different source state")
	}
	if err := verifyTree(target, expected); err != nil {
		return fmt.Errorf("existing Tessarr state failed verification: %w", err)
	}
	return nil
}

func digestManifest(entries []manifestEntry) string {
	hash := sha256.New()
	for _, entry := range entries {
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%o\x00%d\x00%s\n", entry.SourcePath, entry.TargetPath, entry.Mode, entry.Size, entry.Digest)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func pathContains(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != "." && filepath.IsLocal(relative)
}

func samePath(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}
