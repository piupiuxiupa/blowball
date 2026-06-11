package xizhi

import (
	"errors"
	"fmt"
	"os"
)

// readResult is the JSON-serializable result returned by ReadFile.
type readResult struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Size    int    `json:"size"`
}

// ReadFile reads the file at relPath inside workspaceRoot. A missing file is
// reported as ErrFileNotFound so callers can map it to a 404.
func ReadFile(workspaceRoot, relPath string) (any, error) {
	absPath, err := validatePath(workspaceRoot, relPath)
	if err != nil {
		return nil, err
	}
	contents, err := os.ReadFile(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q", ErrFileNotFound, relPath)
		}
		return nil, fmt.Errorf("xizhi read: %q: %w", absPath, err)
	}
	return readResult{Path: relPath, Content: string(contents), Size: len(contents)}, nil
}
