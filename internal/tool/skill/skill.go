// Package skill discovers and reads skill instructions stored in the
// agentskills directory layout: {skill-name}/SKILL.md with YAML frontmatter.
package skill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/lush/blowball/internal/tool"
)

// DefaultMaxSize is the maximum SKILL.md content size read_skill will load.
const DefaultMaxSize int64 = 500 * 1024 // 500KB

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

// List returns all skills available to userID, with user skills overriding
// global skills of the same name. The result is sorted by name.
func (l *Loader) List(userID string) []Skill {
	merged := make(map[string]Skill)
	for _, s := range l.discover(l.globalDir, "global") {
		merged[s.Name] = s
	}
	if userID != "" && l.userDirFn != nil {
		for _, s := range l.discover(l.userDirFn(userID), "user") {
			merged[s.Name] = s
		}
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

// discover scans dir for {name}/SKILL.md entries and parses their frontmatter.
func (l *Loader) discover(dir, location string) []Skill {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		meta, _, err := parseFrontmatter(data)
		if err != nil {
			continue
		}
		if meta.Name == "" || meta.Description == "" {
			continue
		}
		meta.Path = path
		meta.Location = location
		out = append(out, meta)
	}
	return out
}

// parseFrontmatter extracts YAML frontmatter and returns the metadata plus the
// remaining markdown body. It accepts both "---\n...\n---" delimiters.
func parseFrontmatter(data []byte) (Skill, []byte, error) {
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

// Reader is the subset of *Loader needed by read_skill.
type Reader interface {
	Read(name, userID string) ([]byte, error)
}

// RegisterReadSkill registers the read_skill tool into r. Callers should only
// invoke this when at least one agent has skills configured.
func RegisterReadSkill(r *tool.Registry, loader Reader) error {
	spec := &tool.ToolSpec{
		Name:        ToolName,
		Description: "Load a skill by name and return its markdown instructions. User skills take precedence over global skills.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {
					"type": "string",
					"description": "The canonical skill name."
				}
			},
			"required": ["name"],
			"additionalProperties": false
		}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a readArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("read_skill: parse args: %w", err)
			}
			body, err := loader.Read(a.Name, userIDFromContext(ctx))
			if err != nil {
				return nil, err
			}
			return string(body), nil
		},
	}
	return r.Register(spec)
}

// ToolName is the registered name of the read_skill tool.
const ToolName = "read_skill"

type readArgs struct {
	Name string `json:"name"`
}

// contextKey is the type used for context values in this package.
type contextKey int

const userIDKey contextKey = 0

// WithUserID attaches a userID to ctx so read_skill can resolve user skills.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey, userID)
}

func userIDFromContext(ctx context.Context) string {
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
