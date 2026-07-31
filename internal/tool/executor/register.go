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
		Name: ToolBash,
		Description: "Executes a shell command inside a sandboxed workspace and returns `{output, exit_code, truncated}`.\n" +
			"- `output` is combined stdout+stderr; `exit_code` is the process exit status.\n" +
			"- **IMPORTANT: `output` is capped at 64KB** — when truncated it ends with `...output truncated...` and sets " +
			"`truncated: true`; if you need the rest, narrow the command or redirect output to a workspace file and read " +
			"it with `xizhi_read_file`.\n" +
			"- Commands time out at 30s by default.\n" +
			"- The sandbox runs as an unprivileged user with no network by default; only `/workspace` is writable. Global " +
			"skills are read-only at `/skills/global`; per-user skills live at `/workspace/.blowball/skills` (managed via luban).\n" +
			"- **DO NOT use `cat`, `echo`/redirects, `find` or `grep` for file work — use the `xizhi_*` tools** unless a " +
			"dedicated tool cannot do the job.",
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
		Name: ToolPython,
		Description: "Executes Python code or a Python file inside a sandboxed workspace and returns `{output, exit_code, " +
			"truncated}` (same shape, 64KB cap and 30s timeout as `bash`).\n" +
			"- **Exactly one of `code` or `file` is REQUIRED (mutually exclusive).**\n" +
			"- Packages installed under `/workspace/.pip` are available automatically via `PYTHONPATH`.\n" +
			"- The sandbox runs as an unprivileged user with no network by default; only `/workspace` is writable.\n" +
			"- **DO NOT do file I/O from Python — use the `xizhi_*` tools** for reading/writing workspace files unless the " +
			"task needs computation.",
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
		Name: ToolPip,
		Description: "Installs Python packages into the workspace via pip (inside the sandbox) so they are available to the " +
			"`python` tool via `PYTHONPATH`.\n" +
			"- **Use this ONLY to install dependencies — when `python` fails with `ModuleNotFoundError`/`ImportError`.**\n" +
			"- Returns `{output, exit_code, truncated}` (same 64KB output cap as `bash`; 120s timeout by default; network " +
			"enabled by default).\n" +
			"- **DO NOT use this to run code — use `python` for that.**",
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
