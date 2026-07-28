package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/lush/blowball/internal/config"
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

// pipArgs holds the arguments for the pip_install tool.
type pipArgs struct {
	Packages []string `json:"packages"`
	Upgrade  bool     `json:"upgrade"`
}

func registerBash(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name:        ToolBash,
		Description: "Execute a shell command inside a sandboxed workspace. The command runs as an unprivileged user with no network access by default; only the workspace directory is writable. Global skills are mounted read-only at /skills/global; per-user skills live at /workspace/.blowball/skills (read-write, managed via luban).",
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
		Description: "Execute Python code or a Python file inside a sandboxed workspace. Runs as an unprivileged user with no network access by default; only the workspace directory is writable. Installed packages under /workspace/.pip are available automatically via PYTHONPATH. Global skills are mounted read-only at /skills/global; per-user skills live at /workspace/.blowball/skills (read-write, managed via luban).",
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
				return tools.run(ctx, ToolPython, tools.cfg.Python, []string{"python3", resolvePythonFile(a.File)})
			default:
				return nil, fmt.Errorf("python: exactly one of code or file must be provided")
			}
		},
	}
	return r.Register(spec)
}

func registerPip(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name:        ToolPip,
		Description: "Install Python packages into the workspace via pip inside the sandbox. Use this tool when Python code fails with ModuleNotFoundError or ImportError. Packages are installed under /workspace/.pip and are automatically available to the python tool via PYTHONPATH. Network access is enabled by default.",
		ParametersJSON: schemaPip,
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a pipArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("pip_install: parse args: %w", err)
			}
			if len(a.Packages) == 0 {
				return nil, fmt.Errorf("pip_install: at least one package is required")
			}
			for i, pkg := range a.Packages {
				if strings.TrimSpace(pkg) == "" {
					return nil, fmt.Errorf("pip_install: package at index %d is empty", i)
				}
			}
			cmd, err := buildPipCommand(tools.cfg.Pip, a)
			if err != nil {
				return nil, err
			}
			return tools.run(ctx, ToolPip, tools.cfg.Pip.ToExecutorToolConfig(), cmd)
		},
	}
	return r.Register(spec)
}

// buildPipCommand constructs the pip install command for the sandbox.
// It uses "python3 -m pip" instead of the "pip" executable so it works on
// systems where only "pip3" is available or where the "pip" wrapper is missing.
func buildPipCommand(cfg config.PipToolConfig, a pipArgs) ([]string, error) {
	args := []string{"python3", "-m", "pip", "install", "--target", "/workspace/.pip"}
	if a.Upgrade {
		args = append(args, "--upgrade")
	}
	if cfg.IndexURL != "" {
		args = append(args, "-i", cfg.IndexURL)
	}
	for _, url := range cfg.ExtraIndexURLs {
		if url != "" {
			args = append(args, "--extra-index-url", url)
		}
	}
	for _, host := range cfg.TrustedHosts {
		if host != "" {
			args = append(args, "--trusted-host", host)
		}
	}
	args = append(args, a.Packages...)
	return args, nil
}

// resolvePythonFile returns an absolute sandbox path for a Python file argument.
// Relative paths are resolved against /workspace; absolute paths are passed
// through unchanged so scripts in the read-only skill directories can be run.
func resolvePythonFile(file string) string {
	if filepath.IsAbs(file) {
		return file
	}
	return filepath.Join("/workspace", file)
}
