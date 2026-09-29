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

type providerFileCandidate struct {
	file                   File
	baseName               string
	baseKey                string
	fullPath               string
	requiresDisambiguation bool
	logicalName            string
}

// FilesByLogicalName converts provider file records into Tessarr's
// name-keyed representation without losing nested files that share a
// basename. The historical basename key remains unchanged when it is safe and
// unambiguous. Unsafe or colliding basenames receive a stable,
// collision-resistant single-component name. Provider paths remain untouched
// for API lookups while OutputPath carries a separately validated local layout.
func FilesByLogicalName(files []File) (map[string]File, error) {
	if len(files) > maxProviderFileRecords {
		return nil, fmt.Errorf(
			"provider returned %d file records, maximum is %d",
			len(files),
			maxProviderFileRecords,
		)
	}

	candidates := make([]providerFileCandidate, 0, len(files))
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

		originalBaseName := path.Base(fullPath)
		baseName, err := portableProviderFileName(originalBaseName)
		if err != nil {
			return nil, fmt.Errorf("provider file %q: %w", file.Name, err)
		}
		baseKey, _ := safepath.PortableNameKey(baseName)
		file.Path = fullPath
		candidates = append(candidates, providerFileCandidate{
			file:                   file,
			baseName:               baseName,
			baseKey:                baseKey,
			fullPath:               fullPath,
			requiresDisambiguation: baseName != originalBaseName,
		})
		basenameCounts[baseKey]++
	}

	baseOwners := make(map[string][]int, len(candidates))
	queue := make([]int, 0, len(candidates))
	for index := range candidates {
		baseOwners[candidates[index].baseKey] = append(baseOwners[candidates[index].baseKey], index)
		if basenameCounts[candidates[index].baseKey] > 1 {
			candidates[index].requiresDisambiguation = true
		}
		if candidates[index].requiresDisambiguation {
			if err := assignProviderLogicalName(&candidates[index]); err != nil {
				return nil, err
			}
			queue = append(queue, index)
		}
	}

	// Generated names take precedence over a provider-supplied literal that
	// happens to look generated. Promote that literal into the generated
	// namespace too. Each candidate is queued at most once, so even a malicious
	// chain of generated-looking basenames is resolved in linear time.
	for head := 0; head < len(queue); head++ {
		generated := &candidates[queue[head]]
		key, err := safepath.PortableNameKey(generated.logicalName)
		if err != nil {
			return nil, fmt.Errorf("provider file %q: %w", generated.fullPath, err)
		}
		for _, owner := range baseOwners[key] {
			if candidates[owner].requiresDisambiguation {
				continue
			}
			candidates[owner].requiresDisambiguation = true
			if err := assignProviderLogicalName(&candidates[owner]); err != nil {
				return nil, err
			}
			queue = append(queue, owner)
		}
	}
	for index := range candidates {
		if candidates[index].logicalName == "" {
			candidates[index].logicalName = candidates[index].baseName
		}
	}

	result := make(map[string]File, len(candidates))
	portableNames := make(map[string]string, len(candidates))
	portableOutputs := make(map[string]string, len(candidates))
	for index := range candidates {
		candidate := &candidates[index]
		logicalKey, err := safepath.PortableNameKey(candidate.logicalName)
		if err != nil {
			return nil, fmt.Errorf("provider file %q: %w", candidate.fullPath, err)
		}
		if previous, exists := portableNames[logicalKey]; exists {
			return nil, fmt.Errorf("provider files %q and %q have the same generated logical name", previous, candidate.logicalName)
		}
		portableNames[logicalKey] = candidate.logicalName
		outputPath, err := portableProviderOutputPath(candidate.fullPath, candidate.logicalName)
		if err != nil {
			return nil, fmt.Errorf("provider file %q: %w", candidate.fullPath, err)
		}
		outputKey := portableProviderPathKey(outputPath)
		if previous, exists := portableOutputs[outputKey]; exists {
			return nil, fmt.Errorf("provider files %q and %q have the same portable output path", previous, outputPath)
		}
		portableOutputs[outputKey] = outputPath
		candidate.file.Name = candidate.logicalName
		candidate.file.OutputPath = outputPath
		result[candidate.logicalName] = candidate.file
	}
	return result, nil
}

func assignProviderLogicalName(candidate *providerFileCandidate) error {
	candidate.logicalName = candidate.baseName
	if !candidate.requiresDisambiguation {
		return nil
	}
	logicalName, err := disambiguateProviderFileName(candidate.baseName, candidate.fullPath)
	if err != nil {
		return fmt.Errorf("provider file %q: %w", candidate.fullPath, err)
	}
	candidate.logicalName = logicalName
	return nil
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

func portableProviderOutputPath(fullPath, logicalName string) (string, error) {
	parts := strings.Split(fullPath, "/")
	output := make([]string, 0, len(parts))
	for _, component := range parts[:len(parts)-1] {
		portable, err := portableProviderFileName(component)
		if err != nil {
			return "", fmt.Errorf("invalid output directory %q: %w", component, err)
		}
		output = append(output, portable)
	}
	output = append(output, logicalName)
	return strings.Join(output, "/"), nil
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
