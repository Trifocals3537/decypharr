package types

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"unicode"

	"github.com/Trifocals3537/tessarr/internal/safepath"
)

const maxProviderFileRecords = 100_000

// FilesByLogicalName converts provider file records into Tessarr's
// name-keyed representation without losing nested files that share a
// basename. The historical basename key remains unchanged when it is
// unambiguous. Every member of a basename collision group receives a stable,
// collision-safe single-component name while retaining its provider path.
func FilesByLogicalName(files []File) (map[string]File, error) {
	if len(files) > maxProviderFileRecords {
		return nil, fmt.Errorf(
			"provider returned %d file records, maximum is %d",
			len(files),
			maxProviderFileRecords,
		)
	}

	type candidate struct {
		file     File
		baseName string
		baseKey  string
		fullPath string
	}

	candidates := make([]candidate, 0, len(files))
	basenameCounts := make(map[string]int, len(files))
	providerPaths := make(map[string]string, len(files))
	for _, file := range files {
		fullPath, err := normalizeProviderFilePath(file.Path, file.Name)
		if err != nil {
			return nil, fmt.Errorf("provider file %q: %w", file.Name, err)
		}
		pathKey := portableProviderPathKey(fullPath)
		if previous, exists := providerPaths[pathKey]; exists {
			return nil, fmt.Errorf(
				"provider files %q and %q have the same portable path",
				previous,
				fullPath,
			)
		}
		providerPaths[pathKey] = fullPath

		baseName, err := portableProviderFileName(path.Base(fullPath))
		if err != nil {
			return nil, fmt.Errorf("provider file %q: %w", file.Name, err)
		}
		baseKey, _ := safepath.PortableNameKey(baseName)
		file.Path = fullPath
		candidates = append(candidates, candidate{
			file:     file,
			baseName: baseName,
			baseKey:  baseKey,
			fullPath: fullPath,
		})
		basenameCounts[baseKey]++
	}

	result := make(map[string]File, len(candidates))
	portableNames := make(map[string]string, len(candidates))
	for _, candidate := range candidates {
		logicalName := candidate.baseName
		if basenameCounts[candidate.baseKey] > 1 {
			var err error
			logicalName, err = disambiguateProviderFileName(candidate.baseName, candidate.fullPath)
			if err != nil {
				return nil, fmt.Errorf("provider file %q: %w", candidate.fullPath, err)
			}
		}
		logicalKey, err := safepath.PortableNameKey(logicalName)
		if err != nil {
			return nil, fmt.Errorf("provider file %q: %w", candidate.fullPath, err)
		}
		if previous, exists := portableNames[logicalKey]; exists {
			return nil, fmt.Errorf(
				"provider files %q and %q have the same portable logical path",
				previous,
				logicalName,
			)
		}
		portableNames[logicalKey] = logicalName
		candidate.file.Name = logicalName
		result[logicalName] = candidate.file
	}
	return result, nil
}

func portableProviderFileName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("filename is empty")
	}
	if strings.IndexByte(name, 0) >= 0 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("filename contains a NUL byte or control character")
	}
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("filename contains a path separator")
	}

	name = strings.Map(func(r rune) rune {
		switch r {
		case '?', ':', '"', '<', '>', '|', '*':
			return '_'
		default:
			return r
		}
	}, name)
	name = strings.TrimRight(name, " .")
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("filename is empty or traversal after normalization")
	}
	if err := safepath.ValidateIdentifier(name); err != nil {
		prefixed := "file-" + name
		if prefixErr := safepath.ValidateIdentifier(prefixed); prefixErr != nil {
			return "", err
		}
		name = prefixed
	}
	return safepath.CompactIdentifier(name, safepath.PortableIdentifierMaxBytes)
}

func disambiguateProviderFileName(baseName, fullPath string) (string, error) {
	extension := path.Ext(baseName)
	stem := strings.TrimSuffix(baseName, extension)
	digest := sha256.Sum256([]byte(portableProviderPathKey(fullPath)))
	name := stem + "~" + hex.EncodeToString(digest[:]) + extension
	return safepath.CompactIdentifier(name, safepath.PortableIdentifierMaxBytes)
}

func normalizeProviderFilePath(providerPath, fallbackName string) (string, error) {
	value := strings.TrimSpace(providerPath)
	if value == "" {
		value = strings.TrimSpace(fallbackName)
	}
	if value == "" {
		return "", fmt.Errorf("path is empty")
	}
	if strings.IndexByte(value, 0) >= 0 {
		return "", fmt.Errorf("path contains a NUL byte")
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("path contains a control character")
	}

	// Provider APIs commonly prefix torrent-internal paths with a slash. It is
	// a provider-root marker, not a host filesystem absolute path.
	value = strings.ReplaceAll(value, `\`, "/")
	value = strings.TrimLeft(value, "/")
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path traverses outside the provider release")
	}
	return clean, nil
}

func portableProviderPathKey(value string) string {
	parts := strings.Split(strings.ReplaceAll(value, `\`, "/"), "/")
	for i := range parts {
		parts[i] = strings.ToLower(strings.TrimRight(parts[i], " ."))
	}
	return strings.Join(parts, "/")
}
