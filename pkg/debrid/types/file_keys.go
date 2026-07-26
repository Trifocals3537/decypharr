package types

import (
	"fmt"
	"path"
	"strings"
)

const maxProviderFileRecords = 100_000

// FilesByLogicalName converts provider file records into Decypharr's
// name-keyed representation without losing nested files that share a
// basename. The historical basename key remains unchanged when it is
// unambiguous. Every member of a basename collision group instead uses its
// provider-relative path as the logical name and map key.
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
	for _, file := range files {
		fullPath, err := normalizeProviderFilePath(file.Path, file.Name)
		if err != nil {
			return nil, fmt.Errorf("provider file %q: %w", file.Name, err)
		}
		baseName := path.Base(fullPath)
		baseKey := portableProviderPathKey(baseName)
		if baseKey == "" {
			return nil, fmt.Errorf("provider file %q has an empty portable basename", file.Name)
		}
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
			logicalName = candidate.fullPath
		}
		logicalKey := portableProviderPathKey(logicalName)
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
