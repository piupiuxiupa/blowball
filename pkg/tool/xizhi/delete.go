package xizhi

import (
	"errors"
	"fmt"
	"os"
)

// deleteResult is the JSON-serializable result returned by DeletePath.
//
//   - Deleted is true when something existed at the path and was removed; false
//     when the path did not exist (a successful idempotent no-op).
//   - Type is "file", "directory", or "none" (the latter only when nothing
//     existed at the path).
type deleteResult struct {
	Path    string `json:"path"`
	Deleted bool   `json:"deleted"`
	Type    string `json:"type"`
}

// DeletePath deletes the file or directory at relPath inside workspaceRoot.
// Directories are removed recursively. A path that does not exist is treated as
// a successful idempotent no-op (Deleted=false, Type="none"). The reserved
// .blowball namespace and any path that escapes the workspace are rejected by
// validatePath before the filesystem is touched.
func DeletePath(workspaceRoot, relPath string) (any, error) {
	absPath, err := validatePath(workspaceRoot, relPath)
	if err != nil {
		return nil, err
	}
	// Lstat (not Stat) so the reported type reflects the leaf as it is — a
	// symlink is reported as a file and os.RemoveAll removes the link itself,
	// not its target, keeping the report and the effect consistent.
	info, err := os.Lstat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return deleteResult{Path: relPath, Deleted: false, Type: "none"}, nil
		}
		return nil, fmt.Errorf("xizhi delete: stat %q: %w", absPath, err)
	}
	kind := "file"
	if info.IsDir() {
		kind = "directory"
	}
	if err := os.RemoveAll(absPath); err != nil {
		return nil, fmt.Errorf("xizhi delete: %q: %w", absPath, err)
	}
	return deleteResult{Path: relPath, Deleted: true, Type: kind}, nil
}
