package xizhi

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeResult is the JSON-serializable result returned by WriteFile.
type writeResult struct {
	Path string `json:"path"`
	// Size is the file's total size AFTER the write (for append: existing
	// content + appended bytes) so the model can verify growth.
	Size int `json:"size"`
	// Appended is the number of bytes written by THIS call — the full
	// content length in write mode, the appended chunk length in append
	// mode (llm-length-continuation prevention side: chunked writes).
	Appended int    `json:"appended"`
	Absolute string `json:"absolute"`
}

// WriteFile writes content to the file at relPath inside workspaceRoot. With
// append=false (the default mode) it creates or overwrites the file with
// content; with append=true it appends content to the file, creating it (and
// parent directories) when missing — the executable form of the
// split-large-content-into-multiple-writes steering. Parent directories are
// created with mode 0o755 and the file is written with 0o644. The returned
// result echoes the relative path, the post-write total size, the bytes
// written by this call, and the resolved absolute path.
func WriteFile(workspaceRoot, relPath, content string, append bool) (any, error) {
	absPath, err := validatePath(workspaceRoot, relPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return nil, fmt.Errorf("xizhi write: mkdir parents for %q: %w", absPath, err)
	}
	if append {
		f, err := os.OpenFile(absPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("xizhi write: open append %q: %w", absPath, err)
		}
		if _, err := f.WriteString(content); err != nil {
			f.Close()
			return nil, fmt.Errorf("xizhi write: append %q: %w", absPath, err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("xizhi write: close %q: %w", absPath, err)
		}
	} else if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("xizhi write: %q: %w", absPath, err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("xizhi write: stat %q: %w", absPath, err)
	}
	return writeResult{Path: relPath, Size: int(info.Size()), Appended: len(content), Absolute: absPath}, nil
}
