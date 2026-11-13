package xizhi

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// modifyResult is the JSON-serializable result returned by ModifyFile.
type modifyResult struct {
	Path    string `json:"path"`
	OldSize int    `json:"old_size"`
	NewSize int    `json:"new_size"`
}

// ModifyFile replaces the first and only occurrence of oldContent with
// newContent inside the file at relPath. Zero matches is reported as
// ErrOldContentNotFound; more than one match is reported as
// ErrOldContentAmbiguous so the model is forced to disambiguate. A missing
// file surfaces as ErrFileNotFound.
func ModifyFile(workspaceRoot, relPath, oldContent, newContent string) (any, error) {
	absPath, err := validatePath(workspaceRoot, relPath)
	if err != nil {
		return nil, err
	}
	contents, err := os.ReadFile(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q", ErrFileNotFound, relPath)
		}
		return nil, fmt.Errorf("xizhi modify: read %q: %w", absPath, err)
	}

	count := strings.Count(string(contents), oldContent)
	switch count {
	case 0:
		return nil, fmt.Errorf("%w: %q", ErrOldContentNotFound, relPath)
	case 1:
		// fall through — unique replacement below
	default:
		return nil, fmt.Errorf("%w: %d matches in %q", ErrOldContentAmbiguous, count, relPath)
	}

	updated := strings.Replace(string(contents), oldContent, newContent, 1)
	if err := os.WriteFile(absPath, []byte(updated), 0o644); err != nil {
		return nil, fmt.Errorf("xizhi modify: write %q: %w", absPath, err)
	}
	return modifyResult{Path: relPath, OldSize: len(contents), NewSize: len(updated)}, nil
}
