package luban

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/skill"
)

// Registered tool names. These are the strings agents reference in their
// config `tools:` lists and what the registry indexes.
const (
	ToolListSkills     = "luban_list_skills"
	ToolReadSkill      = "luban_read_skill"
	ToolInstallSkill   = "luban_install_skill"
	ToolListSkillFiles = "luban_list_skill_files"
	ToolTreeSkill      = "luban_tree_skill"
)

// Tools holds the dependencies and configuration for the luban skill tools.
type Tools struct {
	loader     *skill.Loader
	userDirFn  func(userID string) string
	httpClient *http.Client
	maxSize    int64
	// market is the optional skill-market client (skill-market capability):
	// the third name-resolution source (user > global > market) and the source
	// of the market entries merged into luban_list_skills. nil = capability
	// off = every tool behaves exactly as before the capability.
	market *skillmarket.Client
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

// WithMarket attaches the skill-market client (the third name-resolution
// source; nil = capability off, the default). The market allowlist is fetched
// lazily per user with the login JWT from the tool-execution context and
// cached for skill_market.cache_ttl; every market failure degrades
// fail-closed to local-only behavior. Chainable, mirroring WithHTTPClient.
func (t *Tools) WithMarket(c *skillmarket.Client) *Tools {
	t.market = c
	return t
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

// RegisterAll registers the luban tools into r.
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
	if err := registerListSkillFiles(r, tools); err != nil {
		return err
	}
	if err := registerTreeSkill(r, tools); err != nil {
		return err
	}
	return nil
}

func registerListSkillFiles(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name: ToolListSkillFiles,
		Description: "Lists the immediate children of a skill's directory (one level, not recursive) and returns " +
			"`{path, entries[]}` inside the standard status envelope (`{\"status\":0,\"result\":{...}}` on success, " +
			"`{\"status\":1,\"error\":...}` on failure); each entry carries `name`, `type` (`file`/`dir`) and `size`. " +
			"**`name` MUST be a simple skill identifier resolved via `luban_list_skills` (user > global > skill market);** " +
			"optional `path` selects a sub-directory relative to the skill root (confined to the skill directory; " +
			"absolute paths, `..` and symlink escapes are rejected). Hidden entries (names starting with `.`) are " +
			"excluded unless `include_hidden` is true, so a git-cloned skill's `.git` is hidden by default. " +
			"**DO NOT list skills with `xizhi_*` — use luban.**",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {
					"type": "string",
					"description": "The canonical skill name. Must be a simple identifier, not a path; resolve it first with luban_list_skills."
				},
				"path": {
					"type": "string",
					"description": "Optional. A sub-directory relative to the skill's directory root to list (defaults to the skill root). Absolute paths, .., and symlinks escaping the skill directory are rejected."
				},
				"include_hidden": {
					"type": "boolean",
					"description": "Whether to include hidden files and directories (names starting with '.'). Defaults to false."
				}
			},
			"required": ["name"],
			"additionalProperties": false
		}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Name          string `json:"name"`
				Path          string `json:"path"`
				IncludeHidden bool   `json:"include_hidden"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("luban_list_skill_files: parse args: %w", err)
			}
			return ListSkillFiles(ctx, tools.loader, tools.market, a.Name, a.Path, skill.UserIDFromContext(ctx), a.IncludeHidden)
		},
	}
	return r.Register(spec)
}

func registerTreeSkill(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name: ToolTreeSkill,
		Description: "Returns a nested tree of a skill's directory and returns `{path, depth, tree[]}` inside the " +
			"standard status envelope (`{\"status\":0,\"result\":{...}}` on success, `{\"status\":1,\"error\":...}` on " +
			"failure); each node carries `name`, `type` (`file`/`dir`), `size` (files only) and `children` (dirs). " +
			"**`name` MUST be a simple skill identifier resolved via `luban_list_skills` (user > global > skill market);** " +
			"optional `path` selects a sub-directory relative to the skill root. `depth` defaults to 3 and is clamped to " +
			"10. Hidden entries (names starting with `.`) are excluded unless `include_hidden` is true. **DO NOT tree " +
			"skills with `xizhi_*` — use luban.**",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {
					"type": "string",
					"description": "The canonical skill name. Must be a simple identifier, not a path; resolve it first with luban_list_skills."
				},
				"path": {
					"type": "string",
					"description": "Optional. A sub-directory relative to the skill's directory root to tree (defaults to the skill root). Absolute paths, .., and symlinks escaping the skill directory are rejected."
				},
				"depth": {
					"type": "integer",
					"description": "Maximum recursion depth. Defaults to 3, maximum 10 (values above 10 are clamped to 10)."
				},
				"include_hidden": {
					"type": "boolean",
					"description": "Whether to include hidden files and directories (names starting with '.'). Defaults to false."
				}
			},
			"required": ["name"],
			"additionalProperties": false
		}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Name          string `json:"name"`
				Path          string `json:"path"`
				Depth         int    `json:"depth"`
				IncludeHidden bool   `json:"include_hidden"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("luban_tree_skill: parse args: %w", err)
			}
			return TreeSkill(ctx, tools.loader, tools.market, a.Name, a.Path, skill.UserIDFromContext(ctx), a.Depth, a.IncludeHidden)
		},
	}
	return r.Register(spec)
}

func registerListSkills(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name: ToolListSkills,
		Description: "List all available skills. The result is delivered inside the standard status envelope: " +
			"`{\"status\":0,\"result\":[{name, description, location}, ...]}` on success (each entry's `location` is " +
			"`global`, `user`, or `skill_market`), or `{\"status\":1,\"error\":...}` on failure. **User skills " +
			"OVERRIDE global skills of the same name; local skills override skill-market skills.** Entries with " +
			"location `skill_market` come from the platform skill market, authorized per user — they are listed " +
			"even when their files have not synced to this host yet (reading an unsynced one returns a clear " +
			"directory-not-found error). **You MUST discover skill names here first, then load one with " +
			"`luban_read_skill` (by name, not path).**",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {},
			"additionalProperties": false
		}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			return listSkills(ctx, tools.loader, tools.market, skill.UserIDFromContext(ctx))
		},
	}
	return r.Register(spec)
}

func registerReadSkill(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name: ToolReadSkill,
		Description: "Reads a skill by name and returns its text body. The result is delivered inside the standard " +
			"status envelope: `{\"status\":0,\"result\":\"<skill text>\"}` on success (a JSON string with YAML " +
			"frontmatter stripped), or `{\"status\":1,\"error\":...}` on failure. Name resolution order is " +
			"user > global > skill market: LOCAL skills take precedence, and skill-market skills (location " +
			"`skill_market` in luban_list_skills) are available as a fallback when no local skill matches. " +
			"**`name` MUST be a simple skill identifier, not a path.** With `path` omitted it reads the " +
			"skill's `SKILL.md`; with `path` provided it reads the text file at that path relative to the skill's " +
			"directory root (confined to the skill directory; any text file is readable, binary files are rejected). " +
			"**DO NOT read skills with `xizhi_*` — use luban.** (Skill-directory access rules live in the system prompt.)",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {
					"type": "string",
					"description": "The canonical skill name. Must be a simple identifier, not a path."
				},
				"path": {
					"type": "string",
					"description": "Optional. A path relative to the skill's directory root pointing at a text file to read (e.g. \"examples/guide.md\", \"templates/config.yaml\", \"scripts/run.py\"). When omitted, the skill's SKILL.md is read. Absolute paths, .., symlinks escaping the skill directory, and binary files are rejected; any other text file is returned verbatim (no frontmatter stripping unless it starts with ---)."
				}
			},
			"required": ["name"],
			"additionalProperties": false
		}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Name string `json:"name"`
				Path string `json:"path"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("luban_read_skill: parse args: %w", err)
			}
			return readSkill(ctx, tools.loader, tools.market, a.Name, a.Path, skill.UserIDFromContext(ctx))
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
			"**IMPORTANT: existing skills with the same name are overwritten**, and all writes stay inside your user skills directory. " +
			"**If a single-file (.md) download fails due to a redirect or non-200 status, you SHOULD retry with the resolved HTTPS URL** — the error includes the HTTP status code and the last redirect Location (you may first call webfetch to discover the final URL and response headers).",
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
