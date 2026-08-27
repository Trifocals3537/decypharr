package storage

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirrobot01/decypharr/internal/utils"
)

const (
	torrentSourceDirName  = "torrent-sources"
	torrentSourceSuffix   = ".torrent"
	torrentSourceDirMode  = 0700
	torrentSourceMode     = 0600
	torrentSourceMaxFiles = 100_000
)

// Keep the durable source cache bounded independently of queue limits. The
// per-file parser ceiling remains 64 MiB; this total ceiling prevents a long-
// lived installation from consuming storage without bound.
var torrentSourceStoreMaxBytes int64 = 1 << 30

func normalizeTorrentSourceHash(infoHash string) (string, error) {
	infoHash = strings.ToLower(strings.TrimSpace(infoHash))
	if len(infoHash) != 40 {
		return "", fmt.Errorf("torrent source infohash must be 40 hexadecimal characters")
	}
	decoded, err := hex.DecodeString(infoHash)
	if err != nil || len(decoded) != 20 {
		return "", fmt.Errorf("torrent source infohash must be 40 hexadecimal characters")
	}
	return infoHash, nil
}

func (s *Storage) torrentSourceDir() string {
	return filepath.Join(s.dir, torrentSourceDirName)
}

func (s *Storage) torrentSourcePath(infoHash string) (string, error) {
	infoHash, err := normalizeTorrentSourceHash(infoHash)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.torrentSourceDir(), infoHash+torrentSourceSuffix), nil
}

func (s *Storage) ensureTorrentSourceDir() error {
	dir := s.torrentSourceDir()
	if err := os.MkdirAll(dir, torrentSourceDirMode); err != nil {
		return fmt.Errorf("create torrent source directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect torrent source directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("torrent source path is not a private directory")
	}
	if err := os.Chmod(dir, torrentSourceDirMode); err != nil {
		return fmt.Errorf("secure torrent source directory: %w", err)
	}
	return nil
}

// SaveTorrentSource validates and durably stores the exact torrent source used
// for provider submission. A mismatched key is rejected so a corrupted or
// substituted file cannot be sent during restart recovery or repair.
func (s *Storage) SaveTorrentSource(infoHash string, data []byte) error {
	infoHash, err := normalizeTorrentSourceHash(infoHash)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("torrent source is empty")
	}
	if int64(len(data)) > utils.MaxMetadataFileBytes {
		return fmt.Errorf("torrent source exceeds %d bytes", utils.MaxMetadataFileBytes)
	}
	parsed, err := utils.GetMagnetFromBytes(data, false)
	if err != nil {
		return fmt.Errorf("validate torrent source: %w", err)
	}
	if parsed.InfoHash != infoHash {
		return fmt.Errorf("torrent source infohash mismatch")
	}

	s.torrentSourcesMu.Lock()
	defer s.torrentSourcesMu.Unlock()

	if err := s.ensureTorrentSourceDir(); err != nil {
		return err
	}
	path, err := s.torrentSourcePath(infoHash)
	if err != nil {
		return err
	}
	var previousSize int64
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("existing torrent source is not a regular file")
		}
		previousSize = info.Size()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect existing torrent source: %w", statErr)
	}
	projectedSize := s.torrentSourceBytes - previousSize + int64(len(data))
	if previousSize > s.torrentSourceBytes || projectedSize > torrentSourceStoreMaxBytes {
		// A full scan is intentionally reserved for startup, explicit cleanup,
		// and quota pressure. Normal admissions stay O(1) even with thousands
		// of retained private-torrent sources.
		total, pruneErr := s.pruneTorrentSourcesLocked(infoHash)
		if pruneErr != nil {
			return pruneErr
		}
		projectedSize = total - previousSize + int64(len(data))
	}
	if projectedSize > torrentSourceStoreMaxBytes {
		return fmt.Errorf("torrent source store exceeds %d bytes", torrentSourceStoreMaxBytes)
	}

	if err := atomicWriteTorrentSource(path, data); err != nil {
		return fmt.Errorf("persist torrent source: %w", err)
	}
	s.torrentSourceBytes = projectedSize
	return nil
}

// LoadTorrentSource returns only a regular, bounded torrent file whose content
// hashes to the requested key. Callers may fall back to a magnet only when the
// source is genuinely absent; all other failures indicate unsafe state.
func (s *Storage) LoadTorrentSource(infoHash string) ([]byte, error) {
	infoHash, err := normalizeTorrentSourceHash(infoHash)
	if err != nil {
		return nil, err
	}
	path, err := s.torrentSourcePath(infoHash)
	if err != nil {
		return nil, err
	}

	s.torrentSourcesMu.Lock()
	defer s.torrentSourcesMu.Unlock()

	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect torrent source: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("torrent source is not a regular file")
	}
	if info.Size() <= 0 || info.Size() > utils.MaxMetadataFileBytes {
		return nil, fmt.Errorf("torrent source size is outside the allowed range")
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open torrent source: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened torrent source: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("torrent source changed while opening")
	}
	data, err := utils.ReadAllLimited(file, utils.MaxMetadataFileBytes)
	if err != nil {
		return nil, fmt.Errorf("read torrent source: %w", err)
	}
	parsed, err := utils.GetMagnetFromBytes(data, false)
	if err != nil {
		return nil, fmt.Errorf("validate stored torrent source: %w", err)
	}
	if parsed.InfoHash != infoHash {
		return nil, fmt.Errorf("stored torrent source infohash mismatch")
	}
	return data, nil
}

// PruneTorrentSources removes sources no longer referenced by either the main
// library or the active queue. It is opportunistic by design: queue and library
// deletion remain independent, while startup and future admissions reclaim
// orphaned files safely.
func (s *Storage) PruneTorrentSources() error {
	s.torrentSourcesMu.Lock()
	defer s.torrentSourcesMu.Unlock()
	total, err := s.pruneTorrentSourcesLocked("")
	if err == nil {
		s.torrentSourceBytes = total
	}
	return err
}

func (s *Storage) pruneTorrentSourcesLocked(keepInfoHash string) (int64, error) {
	dir := s.torrentSourceDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("list torrent sources: %w", err)
	}
	if len(entries) > torrentSourceMaxFiles {
		return 0, fmt.Errorf("torrent source store exceeds %d directory entries", torrentSourceMaxFiles)
	}

	var total int64
	changed := false
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir, name)
		info, infoErr := entry.Info()
		if infoErr != nil {
			return 0, fmt.Errorf("inspect torrent source entry: %w", infoErr)
		}
		if strings.HasPrefix(name, ".") && strings.Contains(name, ".tmp-") {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return 0, fmt.Errorf("remove incomplete torrent source: %w", removeErr)
			}
			changed = true
			continue
		}

		hashName := strings.TrimSuffix(name, torrentSourceSuffix)
		validSourceName := strings.HasSuffix(name, torrentSourceSuffix)
		if _, hashErr := normalizeTorrentSourceHash(hashName); hashErr != nil {
			validSourceName = false
		}
		if validSourceName && hashName != keepInfoHash &&
			!s.entries.Exists(hashName) && !s.queue.Exists(hashName) {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return 0, fmt.Errorf("remove orphaned torrent source: %w", removeErr)
			}
			changed = true
			continue
		}
		if info.Mode().IsRegular() {
			if info.Size() > torrentSourceStoreMaxBytes-total {
				return 0, fmt.Errorf("torrent source store exceeds %d bytes", torrentSourceStoreMaxBytes)
			}
			total += info.Size()
		}
	}
	if changed {
		if err := syncTorrentSourceDirectory(dir); err != nil {
			return 0, fmt.Errorf("sync pruned torrent sources: %w", err)
		}
	}
	return total, nil
}

func atomicWriteTorrentSource(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()

	if err = temp.Chmod(torrentSourceMode); err != nil {
		return err
	}
	if _, err = temp.Write(data); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = replaceTorrentSource(tempPath, path); err != nil {
		return err
	}
	return syncTorrentSourceDirectory(dir)
}
