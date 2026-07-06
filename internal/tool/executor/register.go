package executor

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lush/blowball/internal/tool"
)

// bashArgs holds the arguments for the bash tool.
type bashArgs struct {
	Command string `json:"command"`
}

// pythonArgs holds the arguments for the python tool.
type pythonArgs struct {
	Code string `json:"code"`
	File string `json:"file"`
}

func registerBash(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name:        ToolBash,
		Description: "Execute a shell command inside a sandboxed workspace. The command runs as an unprivileged user with no network access by default; only the workspace directory is writable.",
		ParametersJSON: schemaBash,
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a bashArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("bash: parse args: %w", err)
			}
			if a.Command == "" {
				return nil, fmt.Errorf("bash: command is required")
			}
			return tools.run(ctx, ToolBash, tools.cfg.Bash, []string{"bash", "-c", a.Command})
		},
	}
	return r.Register(spec)
}

func registerPython(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name:        ToolPython,
		Description: "Execute Python code or a Python file inside a sandboxed workspace. Runs as an unprivileged user with no network access by default; only the workspace directory is writable.",
		ParametersJSON: schemaPython,
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a pythonArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("python: parse args: %w", err)
			}
			switch {
			case a.Code != "" && a.File == "":
				return tools.run(ctx, ToolPython, tools.cfg.Python, []string{"python3", "-c", a.Code})
			case a.File != "" && a.Code == "":
				return tools.run(ctx, ToolPython, tools.cfg.Python, []string{"python3", "/workspace/" + a.File})
			default:
				return nil, fmt.Errorf("python: exactly one of code or file must be provided")
			}
		},
	}
	return r.Register(spec)
}
