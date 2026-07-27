package manager

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sirrobot01/decypharr/internal/safepath"
	"github.com/sirrobot01/decypharr/internal/utils"
)

// validateTorrentDownloadFolder accepts the configured download root itself or
// a symlink-free strict descendant. Arbitrary API-supplied save paths must
// never redirect provider results outside the application-owned tree.
func validateTorrentDownloadFolder(configuredRoot, requested string) (string, error) {
	root, err := safepath.ValidateRoot(configuredRoot)
	if err != nil {
		return "", fmt.Errorf("invalid configured download root: %w", err)
	}
	if requested == "" {
		return "", fmt.Errorf("download folder is empty")
	}
	absolute, err := filepath.Abs(requested)
	if err != nil {
		return "", fmt.Errorf("resolve download folder: %w", err)
	}
	absolute = filepath.Clean(absolute)
	relative, err := filepath.Rel(root, absolute)
	if err != nil {
		return "", fmt.Errorf("compare download folder with configured root: %w", err)
	}
	if relative == "." {
		if err := safepath.RejectSymlinks(absolute); err != nil {
			return "", err
		}
		return root, nil
	}
	validated, err := safepath.ValidateUnderRoot(root, absolute)
	if err != nil {
		return "", fmt.Errorf("download folder is outside configured root: %w", err)
	}
	return validated, nil
}

func validateTorrentRootName(name string, allowEmpty bool) error {
	name = strings.TrimSpace(name)
	if name == "" && allowEmpty {
		return nil
	}
	rootName := utils.RemoveExtension(name)
	if err := safepath.ValidateIdentifier(rootName); err != nil {
		return fmt.Errorf("invalid torrent root name %q: %w", name, err)
	}
	return nil
}

func (m *Manager) validateTorrentImportRequest(req *ImportRequest) error {
	if req == nil || req.Magnet == nil {
		return fmt.Errorf("magnet is required")
	}
	if req.Arr == nil {
		return fmt.Errorf("arr is required")
	}
	downloadFolder, err := validateTorrentDownloadFolder(m.config.DownloadFolder, req.DownloadFolder)
	if err != nil {
		return err
	}
	if strings.TrimSpace(req.Arr.Name) == "" {
		req.Arr.Name = "uncategorized"
	}
	if err := safepath.ValidateIdentifier(req.Arr.Name); err != nil {
		return fmt.Errorf("invalid category %q: %w", req.Arr.Name, err)
	}
	if err := validateTorrentRootName(req.Magnet.Name, true); err != nil {
		return err
	}
	req.DownloadFolder = downloadFolder
	return nil
}
