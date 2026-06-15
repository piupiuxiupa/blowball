package xizhi

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/bmatcuk/doublestar/v4"
)

// globResult is the JSON-serializable result returned by GlobFiles.
type globResult struct {
	Path    string   `json:"path"`
	Pattern string   `json:"pattern"`
	Matches []string `json:"matches"`
}

// GlobFiles searches workspaceRoot/relPath for entries matching the doublestar
// glob pattern. An empty pattern returns an empty match list. Hidden entries
// (any path component beginning with ".") are omitted unless includeHidden is
// true. Symlinks are not followed.
func GlobFiles(workspaceRoot, relPath, pattern string, includeHidden bool) (any, error) {
	relPath = normalizePath(relPath)
	absPath, err := validatePath(workspaceRoot, relPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("directory not found: %w", err)
		}
		return nil, fmt.Errorf("xizhi glob_files: stat %q: %w", absPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("xizhi glob_files: %q is not a directory", relPath)
	}

	if pattern == "" {
		return globResult{Path: relPath, Pattern: pattern, Matches: []string{}}, nil
	}

	globPattern := filepath.ToSlash(pattern)
	matches, err := doublestar.Glob(os.DirFS(absPath), globPattern, doublestar.WithNoFollow())
	if err != nil {
		return nil, fmt.Errorf("xizhi glob_files: pattern %q: %w", pattern, err)
	}
	sort.Strings(matches)

	relMatches := make([]string, 0, len(matches))
	for _, match := range matches {
		// doublestar.Glob on an fs.FS returns paths with '/' separators.
		rel := filepath.ToSlash(match)
		if !includeHidden && isHiddenPath(rel) {
			continue
		}
		relMatches = append(relMatches, rel)
	}

	return globResult{Path: relPath, Pattern: pattern, Matches: relMatches}, nil
}
