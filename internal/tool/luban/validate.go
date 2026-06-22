package luban

import (
	"fmt"
	"path/filepath"
	"strings"
)

// validateSkillName treats name as an identifier, not a filesystem path.
// It rejects empty names, absolute paths, "..", and path separators so
// callers can safely join it with a known parent directory.
func validateSkillName(name string) error {
	if name == "" {
		return fmt.Errorf("skill name is empty")
	}
	if filepath.IsAbs(name) {
		return fmt.Errorf("skill name must not be an absolute path")
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("skill name contains invalid sequence %q", "..")
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("skill name must be a simple identifier, not a path")
	}
	return nil
}

// normalizeName strips common suffixes such as ".git" from a name inferred
// from a URL path.
func normalizeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, ".git")
	return name
}
