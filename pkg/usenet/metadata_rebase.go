package usenet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RebaseNZBMetadataPaths rewrites source-NZB paths in an offline migration
// staging tree. It leaves external paths untouched and rejects a contained
// path unless it is the exact source file belonging to that NZB ID.
func RebaseNZBMetadataPaths(metaDir, sourceRoot, targetRoot string) (int, error) {
	changed := 0
	sourceNZBRoot := filepath.Join(sourceRoot, "usenet", "nzbs")
	targetNZBRoot := filepath.Join(targetRoot, "usenet", "nzbs")
	err := scanMetadataDirectory(metaDir, metaReadBatchSize, func(entry os.DirEntry) error {
		if entry.IsDir() || filepath.Ext(entry.Name()) != metaFileExtension {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("NZB metadata %q is a symlink", entry.Name())
		}
		id := strings.TrimSuffix(entry.Name(), metaFileExtension)
		path, err := metadataFilePath(metaDir, id, nzbMetaSuffix)
		if err != nil {
			return err
		}
		info, err := statMetadataFile(metaDir, path)
		if err != nil {
			return fmt.Errorf("inspect NZB metadata %q: %w", id, err)
		}
		data, err := readMetadataFile(metaDir, path)
		if err != nil {
			return fmt.Errorf("read NZB metadata %q: %w", id, err)
		}
		nzb, err := decodeNZB(data)
		if err != nil {
			return fmt.Errorf("decode NZB metadata %q: %w", id, err)
		}
		if err := validateStoredNZBIdentity(id, nzb); err != nil {
			return err
		}
		if nzb.Path == "" || !filepath.IsAbs(nzb.Path) {
			return nil
		}
		relative, err := filepath.Rel(sourceRoot, filepath.Clean(nzb.Path))
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil
		}
		if _, err := validatePersistedMetadataPath(sourceNZBRoot, id, nzb.Path, nzbSourceSuffix); err != nil {
			return fmt.Errorf("unsafe stored source path for NZB %q: %w", id, err)
		}
		nzb.Path = filepath.Join(targetNZBRoot, id+string(nzbSourceSuffix))
		encoded, err := encodeNZBV2(nzb)
		if err != nil {
			return fmt.Errorf("encode NZB metadata %q: %w", id, err)
		}
		temporary, err := metadataFilePath(metaDir, id, nzbMetaV2TempSuffix)
		if err != nil {
			return err
		}
		if err := writeMetadataFile(metaDir, temporary, encoded, info.Mode().Perm()); err != nil {
			return fmt.Errorf("write staged NZB metadata %q: %w", id, err)
		}
		if err := renameMetadataFile(metaDir, temporary, path); err != nil {
			return fmt.Errorf("replace staged NZB metadata %q: %w", id, err)
		}
		changed++
		return nil
	})
	return changed, err
}
