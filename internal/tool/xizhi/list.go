package xizhi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// listEntry is one item returned by xizhi_list_files.
type listEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file" or "dir"
	Size int64  `json:"size"` // meaningful only for files
}

// listResult is the JSON-serializable result returned by ListFiles.
type listResult struct {
	Path    string      `json:"path"`
	Entries []listEntry `json:"entries"`
}

// ListFiles lists the immediate children of relPath inside workspaceRoot. The
// path is normalised so an empty string or "." refers to the workspace root.
// Hidden entries (names beginning with ".") are omitted unless includeHidden is
// true. Non-directory targets and missing directories are reported as errors.
func ListFiles(workspaceRoot, relPath string, includeHidden bool) (any, error) {
	relPath = normalizePath(relPath)
	absPath, err := validatePath(workspaceRoot, relPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("directory not found: %w", err)
		}
		return nil, fmt.Errorf("xizhi list_files: stat %q: %w", absPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("xizhi list_files: %q is not a directory", relPath)
	}

	dirEntries, err := os.ReadDir(absPath)
	if err != nil {
		return nil, fmt.Errorf("xizhi list_files: read dir %q: %w", absPath, err)
	}

	entries := make([]listEntry, 0, len(dirEntries))
	for _, entry := range dirEntries {
		name := entry.Name()
		if !includeHidden && isHiddenName(name) {
			continue
		}

		var typ string
		var size int64
		if entry.IsDir() {
			typ = "dir"
		} else {
			typ = "file"
			fi, err := entry.Info()
			if err != nil {
				return nil, fmt.Errorf("xizhi list_files: entry info %q: %w", name, err)
			}
			size = fi.Size()
		}
		entries = append(entries, listEntry{Name: name, Type: typ, Size: size})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return listResult{Path: relPath, Entries: entries}, nil
}

// normalizePath converts an empty relative path to "." so validatePath accepts
// the workspace root.
func normalizePath(relPath string) string {
	if relPath == "" {
		return "."
	}
	return relPath
}

// isHiddenName reports whether a file or directory name should be considered
// hidden (starts with ".").
func isHiddenName(name string) bool {
	return name != "" && name[0] == '.'
}

// isHiddenPath reports whether any component of relPath is hidden.
func isHiddenPath(relPath string) bool {
	relPath = filepath.ToSlash(relPath)
	for _, part := range strings.Split(relPath, "/") {
		if isHiddenName(part) {
			return true
		}
	}
	return false
}
