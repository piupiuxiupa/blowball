package xizhi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lush/blowball/internal/tool"
)

// Registered tool names. These are the strings agents reference in their
// config `tools:` lists and what the registry indexes.
const (
	NameReadFile   = "xizhi_read_file"
	NameWriteFile  = "xizhi_write_file"
	NameModifyFile = "xizhi_modify_file"
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

// RegisterAll registers the three Xizhi file tools against r, scoping every
// operation at workspaceRoot. It is called once at server bootstrap. A
// duplicate-name registration indicates a programming error and panics here
// rather than silently masking an earlier registration.
func RegisterAll(r *tool.Registry, workspaceRoot string) {
	tools := []*tool.ToolSpec{
		{
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
		},
		{
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
		},
		{
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
		},
	}
	for _, spec := range tools {
		if err := r.Register(spec); err != nil {
			panic(fmt.Sprintf("xizhi register %q: %v", spec.Name, err))
		}
	}
}
