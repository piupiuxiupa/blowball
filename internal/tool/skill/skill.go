// Package skill discovers and reads skill instructions stored in the
// agentskills directory layout: {skill-name}/SKILL.md with YAML frontmatter.
package skill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultMaxSize is the maximum SKILL.md content size the loader will read.
const DefaultMaxSize int64 = 500 * 1024 // 500KB

// MaxDiscoveryDepth limits how deeply discover walks when looking for
// SKILL.md files. Nested skill collections (e.g. superpowers/skills/{name})
// are supported, but depth is capped to avoid accidental deep scans.
const MaxDiscoveryDepth = 5

// Skill holds metadata for a discovered skill.
type Skill struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// Path is the absolute path to the SKILL.md file.
	Path string `yaml:"-"`
	// Location identifies where the skill lives ("global" or "user").
	Location string `yaml:"-"`
}

// Loader discovers skills from a global directory and per-user directories.
type Loader struct {
	globalDir string
	userDirFn func(userID string) string
	maxSize   int64
}

// NewLoader creates a Loader. globalDir is the project-level skills directory.
// userDirFn maps a userID to that user's skills directory. Either may be empty
// if that source is not configured.
func NewLoader(globalDir string, userDirFn func(userID string) string) *Loader {
	return &Loader{
		globalDir: globalDir,
		userDirFn: userDirFn,
		maxSize:   DefaultMaxSize,
	}
}

// WithMaxSize sets the maximum skill file size. It is exposed for tests.
func (l *Loader) WithMaxSize(size int64) *Loader {
	l.maxSize = size
	return l
}

// MaxSize returns the configured size limit.
func (l *Loader) MaxSize() int64 { return l.maxSize }

// GlobalDir returns the project-level skills directory configured at loader
// construction time. It is used by callers that need to expose the global
// skills path to agents or to sandbox mounts.
func (l *Loader) GlobalDir() string { return l.globalDir }

// List returns all skills available to userID, with user skills overriding
// global skills of the same name. The result is sorted by name.
func (l *Loader) List(userID string) []Skill {
	return l.merge(l.ListGlobal(), l.listUser(userID))
}

// Discover walks dir recursively for {skill}/SKILL.md entries and returns the
// skills whose frontmatter has both a name and a description, sorted by name.
// It is the exported form of the recursive discovery used by List, exposed so
// callers that operate on an arbitrary cloned directory (e.g. luban sub-skill
// selection from a cloned collection) can reuse the same discovery logic.
// Location is left empty; callers that need it should use List/ListGlobal.
func (l *Loader) Discover(dir string) []Skill {
	out := l.discover(dir, "")
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListGlobal returns only the global skills discovered from the project-level
// skills directory. The result is sorted by name.
func (l *Loader) ListGlobal() []Skill {
	return l.merge(l.discover(l.globalDir, "global"), nil)
}

// listUser returns only the user skills discovered from the user's skills
// directory. The result is sorted by name.
func (l *Loader) listUser(userID string) []Skill {
	if userID == "" || l.userDirFn == nil {
		return nil
	}
	return l.merge(nil, l.discover(l.userDirFn(userID), "user"))
}

// merge combines global and user skills, with user skills overriding global
// skills of the same name, and returns a sorted slice.
func (l *Loader) merge(global, user []Skill) []Skill {
	merged := make(map[string]Skill)
	for _, s := range global {
		merged[s.Name] = s
	}
	for _, s := range user {
		merged[s.Name] = s
	}
	names := make([]string, 0, len(merged))
	for n := range merged {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Skill, 0, len(names))
	for _, n := range names {
		out = append(out, merged[n])
	}
	return out
}

// HasSkill reports whether a skill with name exists for userID (globally or in
// the user's directory).
func (l *Loader) HasSkill(name, userID string) bool {
	for _, s := range l.List(userID) {
		if s.Name == name {
			return true
		}
	}
	return false
}

// Read returns the markdown body of the named skill, with YAML frontmatter
// stripped. User skills take precedence over global skills.
func (l *Loader) Read(name, userID string) ([]byte, error) {
	for _, s := range l.List(userID) {
		if s.Name != name {
			continue
		}
		info, err := os.Stat(s.Path)
		if err != nil {
			return nil, fmt.Errorf("stat skill %q: %w", name, err)
		}
		if info.Size() > l.maxSize {
			return nil, fmt.Errorf("skill %q exceeds size limit (%d > %d)", name, info.Size(), l.maxSize)
		}
		data, err := os.ReadFile(s.Path)
		if err != nil {
			return nil, fmt.Errorf("read skill %q: %w", name, err)
		}
		_, body, err := parseFrontmatter(data)
		if err != nil {
			return nil, fmt.Errorf("parse skill %q: %w", name, err)
		}
		return body, nil
	}
	return nil, fmt.Errorf("skill %q not found", name)
}

// ReadPath returns the text body of a file located at relPath within the named
// skill's directory tree, with YAML frontmatter stripped if present (only when
// the file begins with `---`, so non-markdown text files are returned verbatim).
// relPath is resolved relative to the matched skill's directory (the directory
// containing its SKILL.md) and MUST stay inside it: absolute paths, parent-
// traversal escapes, and symlinks that resolve outside the skill directory are
// rejected. Any text file in the skill directory tree is readable (no longer
// limited to .md); binary files (content containing a NUL byte, aligning with
// the workspace WriteContent BINARY_FILE judgment) are rejected with a clear
// error so binary garbage never pollutes the context. The named skill is
// resolved the same way as Read (user skills override global skills of the same
// name). The same size limit (DefaultMaxSize unless overridden) applies.
func (l *Loader) ReadPath(name, relPath, userID string) ([]byte, error) {
	for _, s := range l.List(userID) {
		if s.Name != name {
			continue
		}
		skillRoot := filepath.Dir(s.Path)
		absPath, err := validateSkillSubPath(skillRoot, relPath)
		if err != nil {
			return nil, fmt.Errorf("luban_read_skill: %w", err)
		}
		info, err := os.Stat(absPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("luban_read_skill: file not found: %q", relPath)
			}
			return nil, fmt.Errorf("luban_read_skill: stat %q: %w", relPath, err)
		}
		if info.Size() > l.maxSize {
			return nil, fmt.Errorf("luban_read_skill: %q exceeds size limit (%d > %d)", relPath, info.Size(), l.maxSize)
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			return nil, fmt.Errorf("luban_read_skill: read %q: %w", relPath, err)
		}
		// Binary rejection: a NUL byte means the file is binary; return a clear
		// error instead of garbled content (aligns with workspace WriteContent's
		// BINARY_FILE judgment).
		if bytes.IndexByte(data, 0) >= 0 {
			return nil, fmt.Errorf("luban_read_skill: %q is a binary file; only text files are readable", relPath)
		}
		_, body, err := parseFrontmatter(data)
		if err != nil {
			return nil, fmt.Errorf("luban_read_skill: parse %q: %w", relPath, err)
		}
		return body, nil
	}
	return nil, fmt.Errorf("luban_read_skill: skill %q not found", name)
}

// validateSkillSubPath resolves relPath against skillRoot and verifies the real
// path stays inside skillRoot. It rejects absolute paths, parent-traversal
// escapes, and symlinks that resolve outside skillRoot. Any text file in the
// skill directory tree is readable (no extension restriction); binary content
// is rejected by the caller after reading. When the target does not exist it
// returns the joined (non-symlink-resolved) path so the caller's subsequent
// Stat surfaces a clean "file not found".
func validateSkillSubPath(skillRoot, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("path is empty")
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("absolute paths are not allowed; use a path relative to the skill directory such as examples/guide.md")
	}
	cleaned := filepath.Clean(relPath)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path traversal rejected; use a path relative to the skill directory such as examples/guide.md")
	}
	resolvedRoot, err := filepath.EvalSymlinks(skillRoot)
	if err != nil {
		return "", fmt.Errorf("resolve skill directory: %w", err)
	}
	joinedResolved := filepath.Join(resolvedRoot, cleaned)
	resolvedAbs, err := filepath.EvalSymlinks(joinedResolved)
	if err != nil {
		// Target does not exist (or an intermediate parent is missing). Return
		// the non-resolved joined path; the caller's Stat will fail with
		// ErrNotExist and surface "file not found".
		return joinedResolved, nil
	}
	if !pathWithin(resolvedAbs, resolvedRoot) {
		return "", fmt.Errorf("path %q resolves outside the skill directory", relPath)
	}
	return resolvedAbs, nil
}

// pathWithin reports whether target is the same as root or a path beneath
// root, using a separator-aware prefix check so "root-evil" is not treated as
// inside "root".
func pathWithin(target, root string) bool {
	if target == root {
		return true
	}
	return strings.HasPrefix(target, root+string(filepath.Separator))
}

// discover scans dir recursively for SKILL.md entries and parses their
// frontmatter. Directories are walked up to MaxDiscoveryDepth levels.
func (l *Loader) discover(dir, location string) []Skill {
	if dir == "" {
		return nil
	}
	var out []Skill
	var walk func(curDir string, depth int)
	walk = func(curDir string, depth int) {
		if depth > MaxDiscoveryDepth {
			return
		}
		entries, err := os.ReadDir(curDir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			subDir := filepath.Join(curDir, e.Name())
			path := filepath.Join(subDir, "SKILL.md")
			info, err := os.Stat(path)
			if err == nil && !info.IsDir() {
				data, err := os.ReadFile(path)
				if err == nil {
					meta, _, err := parseFrontmatter(data)
					if err == nil && meta.Name != "" && meta.Description != "" {
						meta.Path = path
						meta.Location = location
						out = append(out, meta)
					}
				}
			}
			// Recurse into subdirectories regardless of whether this directory
			// contained a SKILL.md, so nested skill collections are discovered.
			walk(subDir, depth+1)
		}
	}
	walk(dir, 0)
	return out
}

// parseFrontmatter extracts YAML frontmatter and returns the metadata plus the
// remaining markdown body. It accepts both "---\n...\n---" delimiters.
func parseFrontmatter(data []byte) (Skill, []byte, error) {
	return ParseFrontmatter(data)
}

// ParseFrontmatter extracts YAML frontmatter and returns the metadata plus the
// remaining markdown body. It accepts both "---\n...\n---" delimiters.
func ParseFrontmatter(data []byte) (Skill, []byte, error) {
	var meta Skill
	trimmed := bytes.TrimSpace(data)
	if !bytes.HasPrefix(trimmed, []byte("---")) {
		return meta, trimmed, nil
	}
	rest := bytes.TrimPrefix(trimmed, []byte("---"))
	rest = bytes.TrimPrefix(rest, []byte("\n"))
	rest = bytes.TrimPrefix(rest, []byte("\r\n"))
	idx := bytes.Index(rest, []byte("\n---"))
	if idx < 0 {
		idx = bytes.Index(rest, []byte("\r\n---"))
	}
	if idx < 0 {
		return meta, trimmed, fmt.Errorf("unclosed frontmatter")
	}
	if err := yaml.Unmarshal(rest[:idx], &meta); err != nil {
		return meta, nil, fmt.Errorf("unmarshal frontmatter: %w", err)
	}
	body := bytes.TrimSpace(rest[idx+4:])
	return meta, body, nil
}

// contextKey is the type used for context values in this package.
type contextKey int

const userIDKey contextKey = 0

// WithUserID attaches a userID to ctx so skill and executor tools can resolve
// per-user state (e.g. a user's skills directory).
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey, userID)
}

// UserIDFromContext returns the userID previously attached by WithUserID, or
// the empty string if none is present.
func UserIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(userIDKey).(string); ok {
		return v
	}
	return ""
}

// Filter filters skills by the allowed names in names, preserving order.
func Filter(skills []Skill, names []string) []Skill {
	allowed := make(map[string]struct{}, len(names))
	for _, n := range names {
		allowed[n] = struct{}{}
	}
	var out []Skill
	for _, s := range skills {
		if _, ok := allowed[s.Name]; ok {
			out = append(out, s)
		}
	}
	return out
}
