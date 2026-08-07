// Package executor registers the sandboxed bash command execution tool.
//
// Each invocation runs inside a bubblewrap (bwrap) sandbox on Linux. The sandbox
// isolates the command in its own user, mount, pid and (by default) network
// namespaces, binds the user's workspace to /workspace, and restricts the
// inherited environment to variables matching an explicit allow-list.
//
// The dedicated python/pip_install executors were removed; run Python code and
// pip installs through bash. The persistent PYTHONPATH=/workspace/.pip bridge is
// injected into every bash sandbox so pip-via-bash installs remain importable.
//
// On non-Linux platforms the package is a no-op: RegisterAll returns without
// registering tools and IsAvailable reports false.
package executor

import (
	"context"
	"fmt"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/skill"
)

// Tool names registered by this package.
const (
	ToolBash = "bash"
)

// Tools bundles the dependencies required by the executor tools.
type Tools struct {
	cfg             config.ExecutorConfig
	workspaceFn     func(userID string) string
	globalSkillsDir string
	toolsDir        string
}

// NewTools creates an executor tool bundle backed by cfg, workspaceFn,
// globalSkillsDir and toolsDir. workspaceFn maps a userID to the absolute path
// of that user's workspace; per-user skills live under it at
// .blowball/skills and reach the sandbox through the /workspace bind, so no
// separate resolver is needed. globalSkillsDir is the project-level skills
// directory shared across all users, mounted read-only at /skills/global.
// toolsDir is the operator-managed CLI binary directory that is mounted
// read-only at the in-sandbox $HOME/.local/bin.
func NewTools(cfg config.ExecutorConfig, workspaceFn func(userID string) string, globalSkillsDir, toolsDir string) *Tools {
	return &Tools{
		cfg:             cfg,
		workspaceFn:     workspaceFn,
		globalSkillsDir: globalSkillsDir,
		toolsDir:        toolsDir,
	}
}

// RegisterAll registers the bash tool into r when it is enabled in cfg and the
// current platform supports bubblewrap. An error is returned if the tool is
// enabled but cannot be registered (for example, bwrap is missing on Linux).
// (The dedicated python/pip_install executors were removed; Python and pip run
// via bash.)
func RegisterAll(r *tool.Registry, tools *Tools) error {
	if !available {
		return nil
	}

	if tools.cfg.Bash.Enabled {
		if err := registerBash(r, tools); err != nil {
			return err
		}
	}
	return nil
}

// userWorkspace resolves the workspace directory for the user attached to ctx.
// It returns an error when no userID is present or the configured workspaceFn
// is nil.
func (t *Tools) userWorkspace(ctx context.Context) (string, error) {
	userID := skill.UserIDFromContext(ctx)
	if userID == "" {
		return "", fmt.Errorf("executor: userID missing from context")
	}
	if t.workspaceFn == nil {
		return "", fmt.Errorf("executor: workspace resolver not configured")
	}
	return t.workspaceFn(userID), nil
}
