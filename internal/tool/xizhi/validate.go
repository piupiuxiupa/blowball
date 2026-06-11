// Package xizhi implements the Xizhi file tools (read/write/modify) that
// operate inside a user's workspace directory.
//
// Every tool resolves the requested relative path against workspaceRoot and
// applies validatePath to ensure the resolved real path stays inside the
// workspace before touching the filesystem. This application-layer check is
// defence-in-depth alongside the process-level landlock restriction.
package xizhi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Sentinel errors returned by path validation and the file tools. Callers use
// errors.Is to branch on these (e.g. mapping to HTTP status codes).
var (
	// ErrPathOutsideWorkspace is returned when a requested path resolves to a
	// location outside the workspace root (path traversal, absolute path, or a
	// symlink that escapes).
	ErrPathOutsideWorkspace = errors.New("path outside workspace")
	// ErrFileNotFound is returned when a read/modify targets a file that does
	// not exist inside the workspace.
	ErrFileNotFound = errors.New("file not found")
	// ErrOldContentNotFound is returned by ModifyFile when oldContent is absent
	// from the target file.
	ErrOldContentNotFound = errors.New("old content not found")
	// ErrOldContentAmbiguous is returned by ModifyFile when oldContent appears
	// more than once and a unique replacement is therefore impossible.
	ErrOldContentAmbiguous = errors.New("old content is ambiguous, found multiple matches")
)

// IsPathOutsideWorkspace reports whether err is (or wraps) the
// ErrPathOutsideWorkspace sentinel.
func IsPathOutsideWorkspace(err error) bool {
	return errors.Is(err, ErrPathOutsideWorkspace)
}

// IsFileNotFound reports whether err is (or wraps) ErrFileNotFound.
func IsFileNotFound(err error) bool {
	return errors.Is(err, ErrFileNotFound)
}

// ValidatePath is the exported entry point used by the workspace HTTP handler
// to apply the same path-traversal / symlink-escape check the file tools use.
// It is a thin pass-through to the internal validatePath so the workspace
// handler does not duplicate the security logic.
func ValidatePath(workspaceRoot, relPath string) (string, error) {
	return validatePath(workspaceRoot, relPath)
}

// validatePath resolves relPath against workspaceRoot and verifies the real
// (symlink-resolved) path stays inside the workspace. It returns the absolute
// path the caller should operate on, computed as the absolute form of
// filepath.Join(workspaceRoot, relPath). The security check uses a separately
// resolved form so symlinks (including a root that itself sits behind a symlink
// such as /var → /private/var on macOS) cannot trick the prefix comparison.
//
// The check is robust against three classes of escape:
//  1. Absolute paths (filepath.IsAbs) — rejected outright.
//  2. Path traversal — relPath is cleaned first, then any leading ".." segment
//     is rejected.
//  3. Symlink escape — the joined path is EvalSymlinks'd so any symlink at the
//     target or along the parent chain is resolved to its real destination;
//     when the target does not yet exist (a write that creates a new file) the
//     parent directory is resolved instead. The resulting real path is then
//     prefix-checked against the resolved workspace root.
func validatePath(workspaceRoot, relPath string) (string, error) {
	if workspaceRoot == "" {
		return "", fmt.Errorf("%w: workspace root is empty", ErrPathOutsideWorkspace)
	}
	if relPath == "" {
		return "", fmt.Errorf("%w: path is empty", ErrPathOutsideWorkspace)
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("%w: absolute paths are not allowed", ErrPathOutsideWorkspace)
	}

	cleaned := filepath.Clean(relPath)
	// Reject any leading ".." segment. filepath.Clean collapses "../foo" into
	// "../foo" and "/../" into "/", so a leading ".." is the traversal signal.
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path traversal rejected", ErrPathOutsideWorkspace)
	}

	// Absolute form of the requested path; returned to the caller untouched.
	returnPath, err := filepath.Abs(filepath.Join(workspaceRoot, cleaned))
	if err != nil {
		return "", fmt.Errorf("%w: join path: %w", ErrPathOutsideWorkspace, err)
	}

	// Resolve the workspace root through symlinks so a root that is itself a
	// symlink (e.g. /var → /private/var on macOS) is handled correctly. All
	// security checks below operate in this resolved namespace.
	resolvedRoot, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		// Root doesn't exist yet — there is nothing meaningful to validate
		// against, so fail closed.
		return "", fmt.Errorf("%w: resolve workspace root: %w", ErrPathOutsideWorkspace, err)
	}
	joinedResolved := filepath.Join(resolvedRoot, cleaned)

	// Resolve the full path through symlinks. This is the critical step that
	// catches symlinks placed inside the workspace pointing outside: EvalSymlinks
	// follows the link all the way to its real target.
	resolvedAbs, err := filepath.EvalSymlinks(joinedResolved)
	if err != nil {
		// Target doesn't exist (typical for a fresh write). Resolve the parent
		// directory — which must exist for the target to be creatable — and
		// re-append the basename. Symlink escapes via a non-existent parent
		// cannot be exploited because the parent itself cannot exist as a
		// symlink pointing outside.
		parent := filepath.Dir(joinedResolved)
		base := filepath.Base(joinedResolved)
		resolvedParent, perr := filepath.EvalSymlinks(parent)
		if perr != nil {
			// Parent doesn't exist either; fall back to the non-resolved joined
			// path. By construction this is resolvedRoot + cleaned, where
			// cleaned had no leading ".." segment, so it cannot escape via
			// pure path manipulation.
			resolvedAbs = joinedResolved
		} else {
			resolvedAbs = filepath.Join(resolvedParent, base)
		}
	}

	if !isWithin(resolvedAbs, resolvedRoot) {
		return "", fmt.Errorf("%w: %q resolves outside workspace", ErrPathOutsideWorkspace, relPath)
	}
	return returnPath, nil
}

// isWithin reports whether target is the same as root or a path beneath root.
// The boundary check appends a separator so "/data/userA-ws" is not treated as
// inside "/data/userA".
func isWithin(target, root string) bool {
	if target == root {
		return true
	}
	return strings.HasPrefix(target, root+string(os.PathSeparator))
}
