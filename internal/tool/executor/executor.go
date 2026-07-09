// Package executor registers sandboxed bash, python and pip command execution tools.
//
// Each invocation runs inside a bubblewrap (bwrap) sandbox on Linux. The sandbox
// isolates the command in its own user, mount, pid and (by default) network
// namespaces, binds the user's workspace to /workspace, and restricts the
// inherited environment to variables matching an explicit allow-list.
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
	ToolBash   = "bash"
	ToolPython = "python"
	ToolPip    = "pip_install"
)

// Tools bundles the dependencies required by the executor tools.
type Tools struct {
	cfg             config.ExecutorConfig
	workspaceFn     func(userID string) string
	globalSkillsDir string
	userSkillsFn    func(userID string) string
}

// NewTools creates an executor tool bundle backed by cfg, workspaceFn,
// globalSkillsDir and userSkillsFn. workspaceFn and userSkillsFn map a userID
// to the absolute path of that user's workspace and skills directory,
// respectively. globalSkillsDir is the project-level skills directory shared
// across all users.
func NewTools(cfg config.ExecutorConfig, workspaceFn, userSkillsFn func(userID string) string, globalSkillsDir string) *Tools {
	return &Tools{
		cfg:             cfg,
		workspaceFn:     workspaceFn,
		globalSkillsDir: globalSkillsDir,
		userSkillsFn:    userSkillsFn,
	}
}

// RegisterAll registers the bash, python and pip_install tools into r when they
// are enabled in cfg and the current platform supports bubblewrap. An error is
// returned if a tool is enabled but cannot be registered (for example, bwrap is
// missing on Linux).
func RegisterAll(r *tool.Registry, tools *Tools) error {
	if !available {
		return nil
	}

	if tools.cfg.Bash.Enabled {
		if err := registerBash(r, tools); err != nil {
			return err
		}
	}
	if tools.cfg.Python.Enabled {
		if err := registerPython(r, tools); err != nil {
			return err
		}
	}
	if tools.cfg.Pip.Enabled {
		if err := registerPip(r, tools); err != nil {
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

// userSkillsDir resolves the per-user skills directory for the user attached
// to ctx. It returns an error when no userID is present or the configured
// userSkillsFn is nil.
func (t *Tools) userSkillsDir(ctx context.Context) (string, error) {
	userID := skill.UserIDFromContext(ctx)
	if userID == "" {
		return "", fmt.Errorf("executor: userID missing from context")
	}
	if t.userSkillsFn == nil {
		return "", fmt.Errorf("executor: user skills resolver not configured")
	}
	return t.userSkillsFn(userID), nil
}
