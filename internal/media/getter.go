package media

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var sensitiveFilePatterns = []string{
	".env",
	".env.*",
	"*.pem",
	"*.key",
	"*.p12",
	"*.pfx",
	"id_rsa*",
	"id_ed25519*",
	"id_ecdsa*",
	"id_dsa*",
	"*.secret*",
	"credentials.json",
}

// ValidateGetPath validates and resolves a file path for the /get command.
// It enforces directory confinement (no path traversal) and blocks sensitive files.
func ValidateGetPath(projectDir, reqPath string, maxSizeBytes int64) (string, error) {
	if strings.TrimSpace(reqPath) == "" {
		return "", fmt.Errorf("path cannot be empty")
	}

	cleanProjectDir, err := filepath.Abs(projectDir)
	if err != nil {
		return "", fmt.Errorf("resolving project path: %w", err)
	}

	// Resolve target path relative to projectDir
	targetPath := reqPath
	if !filepath.IsAbs(targetPath) {
		targetPath = filepath.Join(cleanProjectDir, reqPath)
	}
	cleanTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return "", fmt.Errorf("resolving target path: %w", err)
	}

	// Enforce directory confinement
	rel, err := filepath.Rel(cleanProjectDir, cleanTarget)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return "", fmt.Errorf("access denied: path escapes project folder")
	}

	// Check if path is inside .git/
	pathParts := strings.Split(filepath.ToSlash(rel), "/")
	for _, part := range pathParts {
		if part == ".git" {
			return "", fmt.Errorf("access denied: .git folder is protected")
		}
	}

	// Check sensitive file patterns
	filename := filepath.Base(cleanTarget)
	for _, pattern := range sensitiveFilePatterns {
		matched, _ := filepath.Match(pattern, filename)
		if matched || filename == pattern {
			return "", fmt.Errorf("access denied: file %q matches sensitive file deny-list", filename)
		}
	}

	// Check file existence & size
	fi, err := os.Stat(cleanTarget)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file not found: %s", rel)
		}
		return "", fmt.Errorf("checking file: %w", err)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("path is a directory, not a file: %s", rel)
	}

	if maxSizeBytes > 0 && fi.Size() > maxSizeBytes {
		return "", fmt.Errorf("file size (%d bytes) exceeds allowed limit (%d bytes)", fi.Size(), maxSizeBytes)
	}

	return cleanTarget, nil
}
