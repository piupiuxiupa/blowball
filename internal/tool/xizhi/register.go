package xizhi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"
)

// Registered tool names. These are the strings agents reference in their
// config `tools:` lists and what the registry indexes.
const (
	NameReadFile   = "xizhi_read_file"
	NameWriteFile  = "xizhi_write_file"
	NameModifyFile = "xizhi_modify_file"
	NameListFiles  = "xizhi_list_files"
	NameTree       = "xizhi_tree"
	NameGlobFiles  = "xizhi_glob_files"
	NameGrep       = "xizhi_grep"
	NameDeleteFile = "xizhi_delete"
)

// Per-tool parameter JSON Schemas. They are JSON objects (not wrapped in an
// array) so they can be emitted verbatim under OpenAI's "parameters" key.
var (
	schemaRead = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path of the file to read, relative to the workspace root."
    }
  },
  "required": ["path"],
  "additionalProperties": false
}`)

	schemaWrite = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Destination path relative to the workspace root. Parent directories are created automatically."
    },
    "content": {
      "type": "string",
      "description": "Full text content to write. Overwrites any existing file at this path."
    }
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`)

	schemaModify = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path of the file to modify, relative to the workspace root."
    },
    "old_content": {
      "type": "string",
      "description": "Exact text to replace. Must occur exactly once in the file."
    },
    "new_content": {
      "type": "string",
      "description": "Replacement text."
    }
  },
  "required": ["path", "old_content", "new_content"],
  "additionalProperties": false
}`)

	schemaList = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Directory path relative to the workspace root. Defaults to the workspace root."
    },
    "include_hidden": {
      "type": "boolean",
      "description": "Whether to include hidden files and directories (names starting with '.'). Defaults to false."
    }
  },
  "required": ["path"],
  "additionalProperties": false
}`)

	schemaTree = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Directory path relative to the workspace root. Defaults to the workspace root."
    },
    "depth": {
      "type": "integer",
      "description": "Maximum recursion depth. Defaults to 3, maximum 10."
    },
    "include_hidden": {
      "type": "boolean",
      "description": "Whether to include hidden files and directories (names starting with '.'). Defaults to false."
    }
  },
  "required": ["path"],
  "additionalProperties": false
}`)

	schemaGlob = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Directory path relative to the workspace root to search within. Defaults to the workspace root."
    },
    "pattern": {
      "type": "string",
      "description": "doublestar glob pattern such as 'src/**/*.go' or '**/*_test.go'."
    },
    "include_hidden": {
      "type": "boolean",
      "description": "Whether to include hidden files and directories (names starting with '.'). Defaults to false."
    }
  },
  "required": ["path", "pattern"],
  "additionalProperties": false
}`)

	schemaGrep = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Directory path relative to the workspace root to search within. Defaults to the workspace root."
    },
    "pattern": {
      "type": "string",
      "description": "RE2 regular expression to match against file contents (e.g. 'func Foo\\(' or 'TODO')."
    },
    "glob": {
      "type": "string",
      "description": "Optional doublestar file-name filter such as '*.go' or '*.py'; only files whose base name matches are searched."
    },
    "ignore_case": {
      "type": "boolean",
      "description": "Whether to match case-insensitively. Defaults to false."
    },
    "include_hidden": {
      "type": "boolean",
      "description": "Whether to include hidden files and directories (names starting with '.'). Defaults to false."
    },
    "context_before": {
      "type": "integer",
      "description": "Number of lines to include before each match. Defaults to 0."
    },
    "context_after": {
      "type": "integer",
      "description": "Number of lines to include after each match. Defaults to 0."
    }
  },
  "required": ["pattern"],
  "additionalProperties": false
}`)

	schemaDelete = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path of the file or directory to delete, relative to the workspace root."
    }
  },
  "required": ["path"],
  "additionalProperties": false
}`)
)

// readArgs / writeArgs / modifyArgs decode the model-supplied tool arguments.
type readArgs struct {
	Path string `json:"path"`
}
type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
type modifyArgs struct {
	Path       string `json:"path"`
	OldContent string `json:"old_content"`
	NewContent string `json:"new_content"`
}

// listArgs / treeArgs / globArgs decode arguments for the discovery tools.
type listArgs struct {
	Path          string `json:"path"`
	IncludeHidden bool   `json:"include_hidden"`
}
type treeArgs struct {
	Path          string `json:"path"`
	Depth         int    `json:"depth"`
	IncludeHidden bool   `json:"include_hidden"`
}
type globArgs struct {
	Path          string `json:"path"`
	Pattern       string `json:"pattern"`
	IncludeHidden bool   `json:"include_hidden"`
}

// grepArgs decodes arguments for xizhi_grep.
type grepArgs struct {
	Path          string `json:"path"`
	Pattern       string `json:"pattern"`
	Glob          string `json:"glob"`
	IgnoreCase    bool   `json:"ignore_case"`
	IncludeHidden bool   `json:"include_hidden"`
	ContextBefore int    `json:"context_before"`
	ContextAfter  int    `json:"context_after"`
}

type deleteArgs struct {
	Path string `json:"path"`
}

// RegisterAll registers the enabled Xizhi file tools against r, scoping every
// operation at workspaceRoot. It is called once per request so each user gets a
// registry bound to their own workspace. A duplicate-name registration indicates
// a programming error and panics here rather than silently masking an earlier
// registration.
func RegisterAll(r *tool.Registry, workspaceRoot string, cfg config.XizhiConfig) {
	var tools []*tool.ToolSpec

	// The original three file tools are always registered for backward
	// compatibility; deployments that want to disable them can do so via the
	// config by leaving them out of agent tool lists.
	tools = append(tools, &tool.ToolSpec{
		Name: NameReadFile,
		Description: "Reads a workspace file and returns `{path, content, size}` with the full contents as a UTF-8 string — " +
			"no line-number prefix, no truncation. **`path` MUST be relative to the workspace root** (absolute paths, " +
			"`..` and symlink escapes are rejected); a missing file returns an error. **DO NOT read files with `bash`/" +
			"`python` (`cat`) — use this tool.**",
		ParametersJSON: schemaRead,
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a readArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("xizhi_read_file: parse args: %w", err)
			}
			return ReadFile(workspaceRoot, a.Path)
		},
	})

	tools = append(tools, &tool.ToolSpec{
		Name: NameWriteFile,
		Description: "Creates or overwrites a workspace file with the given text content and returns `{path, size, absolute}`. " +
			"Parent directories are created automatically. **IMPORTANT: an existing file at `path` is overwritten.** " +
			"**DO NOT write files with `bash`/`python` (`echo`/redirects) — use this tool.**",
		ParametersJSON: schemaWrite,
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a writeArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("xizhi_write_file: parse args: %w", err)
			}
			return WriteFile(workspaceRoot, a.Path, a.Content)
		},
	})

	tools = append(tools, &tool.ToolSpec{
		Name: NameModifyFile,
		Description: "Performs an exact string replacement in a workspace file and returns `{path, old_size, new_size}`. " +
			"**`old_content` MUST match exactly one location** — the call fails if the match is missing or appears more " +
			"than once. **DO NOT rewrite the whole file** — use this for targeted edits and `xizhi_write_file` for full " +
			"rewrites.",
		ParametersJSON: schemaModify,
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a modifyArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("xizhi_modify_file: parse args: %w", err)
			}
			return ModifyFile(workspaceRoot, a.Path, a.OldContent, a.NewContent)
		},
	})

	if cfg.ListFiles.Enabled {
		tools = append(tools, &tool.ToolSpec{
			Name: NameListFiles,
			Description: "Lists the immediate children of a workspace directory and returns `{path, entries[]}` where each " +
				"entry carries `name`, `type` (`file`/`directory`) and `size`. **`path` MUST be relative to the workspace " +
				"root.** **This lists one level only — DO NOT expect recursion; use `xizhi_tree` for nested listings.** " +
				"Hidden entries (names starting with `.`) are excluded unless `include_hidden` is true.",
			ParametersJSON: schemaList,
			Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a listArgs
				if err := json.Unmarshal(args, &a); err != nil {
					return nil, fmt.Errorf("xizhi_list_files: parse args: %w", err)
				}
				return ListFiles(workspaceRoot, a.Path, a.IncludeHidden)
			},
		})
	}

	if cfg.Tree.Enabled {
		tools = append(tools, &tool.ToolSpec{
			Name: NameTree,
			Description: "Returns `{path, depth, tree[]}`, a nested representation of a workspace directory. **`path` MUST " +
				"be relative to the workspace root.** **`depth` defaults to 3 and MUST NOT exceed 10.** Hidden entries are " +
				"excluded unless `include_hidden` is true.",
			ParametersJSON: schemaTree,
			Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a treeArgs
				if err := json.Unmarshal(args, &a); err != nil {
					return nil, fmt.Errorf("xizhi_tree: parse args: %w", err)
				}
				return Tree(workspaceRoot, a.Path, a.Depth, a.IncludeHidden)
			},
		})
	}

	if cfg.GlobFiles.Enabled {
		tools = append(tools, &tool.ToolSpec{
			Name: NameGlobFiles,
			Description: "Searches a workspace directory with a doublestar glob pattern and returns `{path, pattern, " +
				"matches[]}` of relative paths. **`path` MUST be relative to the workspace root.** **`pattern` MUST be a " +
				"doublestar pattern** (e.g. `src/**/*.go`, `**/*_test.go`); hidden entries are excluded unless " +
				"`include_hidden` is true.",
			ParametersJSON: schemaGlob,
			Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a globArgs
				if err := json.Unmarshal(args, &a); err != nil {
					return nil, fmt.Errorf("xizhi_glob_files: parse args: %w", err)
				}
				return GlobFiles(workspaceRoot, a.Path, a.Pattern, a.IncludeHidden)
			},
		})
	}

	if cfg.Grep.Enabled {
		tools = append(tools, &tool.ToolSpec{
			Name: NameGrep,
			Description: "Searches workspace file contents with an RE2 regex and returns `{path, pattern, glob, " +
				"ignore_case, matches[]}` where each match carries `file`, `line_number`, `line`, and (when requested) " +
				"`context_before`/`context_after`. **`path` MUST be relative to the workspace root** (absolute paths, `..` " +
				"and the `.blowball` namespace are rejected). **Prefer this over `bash grep`** — it is cheaper and returns " +
				"line numbers. Binary files are skipped; the result is capped (~200 matches, lines truncated) and sets " +
				"`truncated: true` when the cap is hit. Use `glob` to filter by file name (e.g. `*.go`).",
			ParametersJSON: schemaGrep,
			Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a grepArgs
				if err := json.Unmarshal(args, &a); err != nil {
					return nil, fmt.Errorf("xizhi_grep: parse args: %w", err)
				}
				return GrepFiles(workspaceRoot, a.Path, a.Pattern, a.Glob, a.IgnoreCase, a.IncludeHidden, a.ContextBefore, a.ContextAfter)
			},
		})
	}

	if cfg.Delete.Enabled {
		tools = append(tools, &tool.ToolSpec{
			Name: NameDeleteFile,
			Description: "Deletes a workspace file or directory and returns `{path, deleted, type}` (type is \"file\", " +
				"\"directory\", or \"none\"). A directory is removed recursively; a path that does not exist is a " +
				"successful no-op (`deleted: false`, `type: \"none\"`). **`path` MUST be relative to the workspace root** " +
				"(absolute paths, `..` and symlink escapes are rejected). Use this to clean up scratch/intermediate files " +
				"under `tmp/` once they have served their purpose.",
			ParametersJSON: schemaDelete,
			Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a deleteArgs
				if err := json.Unmarshal(args, &a); err != nil {
					return nil, fmt.Errorf("xizhi_delete: parse args: %w", err)
				}
				return DeletePath(workspaceRoot, a.Path)
			},
		})
	}

	for _, spec := range tools {
		if err := r.Register(spec); err != nil {
			panic(fmt.Sprintf("xizhi register %q: %v", spec.Name, err))
		}
	}
}
