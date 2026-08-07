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

func registerBash(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name: ToolBash,
		Description: "Executes a shell command inside a sandboxed workspace and returns `{output, exit_code, truncated}`.\n" +
			"- `output` is combined stdout+stderr; `exit_code` is the process exit status.\n" +
			"- **IMPORTANT: `output` is capped at 64KB** — when truncated it ends with `...output truncated...` and sets " +
			"`truncated: true`; if you need the rest, narrow the command or redirect output to a workspace file and read " +
			"it with `xizhi_read_file`.\n" +
			"- **MUST: commands time out at 30s by default.**\n" +
			"- The sandbox runs as an unprivileged user; only `/workspace` is writable and network is enabled by default " +
			"(operator may disable via config). Global skills are read-only at `/skills/global`; per-user skills live at " +
			"`/workspace/.blowball/skills` (managed via luban).\n" +
			"- Run Python here with `python3 ...` and install packages with `python3 -m pip install --target " +
			"/workspace/.pip <pkg>`; installed packages are importable in later runs via the injected `PYTHONPATH` bridge.\n" +
			"- **DO NOT use `cat`, `rm`, `ls`, `find`, `sed`, `awk` or `grep` for workspace file work — use the dedicated " +
			"`xizhi_*` tools instead** (`cat`→`xizhi_read_file`; `ls`→`xizhi_list_files`/`xizhi_tree`; " +
			"`find`→`xizhi_glob_files`; `grep`→`xizhi_grep`; `sed`/`awk`→`xizhi_modify_file`; `rm`→`xizhi_delete`), " +
			"unless a dedicated tool genuinely cannot do the job.",
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
