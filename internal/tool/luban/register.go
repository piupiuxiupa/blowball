package luban

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/skill"
)

// Registered tool names. These are the strings agents reference in their
// config `tools:` lists and what the registry indexes.
const (
	ToolListSkills   = "luban_list_skills"
	ToolReadSkill    = "luban_read_skill"
	ToolInstallSkill = "luban_install_skill"
)

// Tools holds the dependencies and configuration for the luban skill tools.
type Tools struct {
	loader     *skill.Loader
	userDirFn  func(userID string) string
	httpClient *http.Client
	maxSize    int64
}

// NewTools creates a luban tool bundle backed by loader and userDirFn.
func NewTools(loader *skill.Loader, userDirFn func(userID string) string) *Tools {
	return &Tools{
		loader:     loader,
		userDirFn:  userDirFn,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		maxSize:    MaxInstallSize,
	}
}

// WithHTTPClient overrides the HTTP client used for single-file downloads.
// Exposed for tests.
func (t *Tools) WithHTTPClient(c *http.Client) *Tools {
	t.httpClient = c
	return t
}

// WithMaxSize overrides the maximum download size. Exposed for tests.
func (t *Tools) WithMaxSize(size int64) *Tools {
	t.maxSize = size
	return t
}

// RegisterAll registers the three luban tools into r.
func RegisterAll(r *tool.Registry, tools *Tools) error {
	if err := registerListSkills(r, tools); err != nil {
		return err
	}
	if err := registerReadSkill(r, tools); err != nil {
		return err
	}
	if err := registerInstallSkill(r, tools); err != nil {
		return err
	}
	return nil
}

func registerListSkills(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name:        ToolListSkills,
		Description: "List all available skills, both global and user-specific. User skills override global skills of the same name. Use luban_list_skills to discover skills, then luban_read_skill to load one. Never use xizhi_* tools to access the skills directory.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {},
			"additionalProperties": false
		}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			return listSkills(tools.loader, skill.UserIDFromContext(ctx))
		},
	}
	return r.Register(spec)
}

func registerReadSkill(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name:        ToolReadSkill,
		Description: "Read a skill by name and return its markdown instructions. User skills take precedence over global skills. Use luban_read_skill, not xizhi_read_file, to access skills. Never use xizhi_* tools to access the skills directory.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {
					"type": "string",
					"description": "The canonical skill name. Must be a simple identifier, not a path."
				}
			},
			"required": ["name"],
			"additionalProperties": false
		}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("luban_read_skill: parse args: %w", err)
			}
			return readSkill(tools.loader, a.Name, skill.UserIDFromContext(ctx))
		},
	}
	return r.Register(spec)
}

func registerInstallSkill(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name: ToolInstallSkill,
		Description: "Install a skill or skill collection from a URL into your user skills directory. luban_install_skill handles four shapes: " +
			"(1) a whole git repository is cloned and installed as one entry (its root SKILL.md, or the sub-skills it contains); " +
			"(2) a git repository that is a collection of sub-skills, combined with the optional `skill` parameter, installs only the selected sub-skill (matched by frontmatter `name`, else by repo-relative subpath) and discards the rest of the clone; " +
			"(3) a URL ending in .md pointing at a single SKILL.md is downloaded and installed directly; " +
			"(4) a .md URL whose body is NOT a valid SKILL.md is returned as an install document (result kind \"install-doc\") carrying the fetched content and a hint - read it, find the real skill source it describes, and call luban_install_skill again with that source URL. " +
			"Existing skills with the same name are overwritten. All writes stay inside your user skills directory. Never use xizhi_* tools to access the skills directory.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"url": {
					"type": "string",
					"description": "HTTPS URL of the skill collection (e.g. a GitHub repo), a single SKILL.md file, or an install-documentation page ending in .md."
				},
				"name": {
					"type": "string",
					"description": "Optional target skill name. If omitted, the name is inferred from the URL path (for single files and whole repos) or from the selected sub-skill's frontmatter name (for sub-skill selection)."
				},
				"skill": {
					"type": "string",
					"description": "Optional. For git-repo collection URLs only: select a single sub-skill to install. Matched against a discovered sub-skill's frontmatter name; if there is no unique name match, treated as a repo-relative subpath (e.g. \"skills/my-skill\") whose directory contains a SKILL.md. On no match the tool errors with the list of available sub-skill names. Ignored for single-file sources."
				}
			},
			"required": ["url"],
			"additionalProperties": false
		}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				URL   string `json:"url"`
				Name  string `json:"name"`
				Skill string `json:"skill"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("luban_install_skill: parse args: %w", err)
			}
			ins := newInstaller(tools.loader, tools.userDirFn)
			ins.httpClient = tools.httpClient
			ins.maxSize = tools.maxSize
			return ins.installSkill(ctx, a.URL, a.Name, a.Skill, skill.UserIDFromContext(ctx))
		},
	}
	return r.Register(spec)
}
