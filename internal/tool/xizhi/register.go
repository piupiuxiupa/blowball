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
		Name:           NameReadFile,
		Description:    "Read a file from the user's workspace. The path must be relative to the workspace root.",
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
		Name:           NameWriteFile,
		Description:    "Write (create or overwrite) a file inside the user's workspace. Parent directories are created automatically.",
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
		Name:           NameModifyFile,
		Description:    "Replace a unique occurrence of text in a workspace file. Fails if old_content is missing or appears more than once.",
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
			Name:           NameListFiles,
			Description:    "List the files and directories in a workspace directory. Returns name, type, and file size. Hidden entries are excluded by default.",
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
			Name:           NameTree,
			Description:    "Return a nested directory tree for a workspace path. Default depth is 3 and maximum depth is 10. Hidden entries are excluded by default.",
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
			Name:           NameGlobFiles,
			Description:    "Search the workspace for files and directories matching a doublestar glob pattern such as 'src/**/*.go'. Returns a list of relative paths. Hidden entries are excluded by default.",
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

	for _, spec := range tools {
		if err := r.Register(spec); err != nil {
			panic(fmt.Sprintf("xizhi register %q: %v", spec.Name, err))
		}
	}
}
