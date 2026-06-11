package xizhi

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeResult is the JSON-serializable result returned by WriteFile.
type writeResult struct {
	Path     string `json:"path"`
	Size     int    `json:"size"`
	Absolute string `json:"absolute"`
}

// WriteFile writes content to the file at relPath inside workspaceRoot. Parent
// directories are created with mode 0o755 and the file is written with 0o644.
// The returned result echoes the relative path, byte size and resolved
// absolute path.
func WriteFile(workspaceRoot, relPath, content string) (any, error) {
	absPath, err := validatePath(workspaceRoot, relPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return nil, fmt.Errorf("xizhi write: mkdir parents for %q: %w", absPath, err)
	}
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("xizhi write: %q: %w", absPath, err)
	}
	return writeResult{Path: relPath, Size: len(content), Absolute: absPath}, nil
}
