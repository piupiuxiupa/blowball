//go:build linux

package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/executor"
	"github.com/lush/blowball/internal/tool/skill"
)

func init() {
	// Integration tests use the no-op default logger.
	logger.SetDefault(logger.L())
}

func newRegistryWithExecutor(t *testing.T, tools *executor.Tools) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry()
	require.NoError(t, executor.RegisterAll(reg, tools))
	return reg
}

func newExecutorTools(t *testing.T, cfg config.ExecutorConfig) (*executor.Tools, string) {
	t.Helper()
	ws := t.TempDir()
	globalSkills := t.TempDir()
	tools := t.TempDir()
	return executor.NewTools(cfg, func(userID string) string {
		return ws
	}, globalSkills, tools), ws
}

func executorCtx(userID string) context.Context {
	return skill.WithUserID(context.Background(), userID)
}

func TestExecutorBashEcho(t *testing.T) {
	if !executor.IsAvailable() {
		t.Skip("bwrap not available")
	}

	tools, _ := newExecutorTools(t, config.ExecutorConfig{
		Bash: config.ExecutorToolConfig{Enabled: true, Timeout: defaultTimeout, MaxOutputBytes: 4096},
	})
	reg := newRegistryWithExecutor(t, tools)

	res, err := reg.Call(executorCtx("u1"), executor.ToolBash, json.RawMessage(`{"command":"echo hello"}`))
	require.NoError(t, err)
	m := res.(*executor.ExecutionResult)
	require.Equal(t, "hello\n", m.Output)
	require.Equal(t, 0, m.ExitCode)
	require.False(t, m.Truncated)
}

func TestExecutorPythonCode(t *testing.T) {
	if !executor.IsAvailable() {
		t.Skip("bwrap not available")
	}

	tools, _ := newExecutorTools(t, config.ExecutorConfig{
		Python: config.ExecutorToolConfig{Enabled: true, Timeout: defaultTimeout, MaxOutputBytes: 4096},
	})
	reg := newRegistryWithExecutor(t, tools)

	res, err := reg.Call(executorCtx("u1"), executor.ToolPython, json.RawMessage(`{"code":"print(1+1)"}`))
	require.NoError(t, err)
	m := res.(*executor.ExecutionResult)
	require.Equal(t, "2\n", m.Output)
	require.Equal(t, 0, m.ExitCode)
}

func TestExecutorPythonFile(t *testing.T) {
	if !executor.IsAvailable() {
		t.Skip("bwrap not available")
	}

	tools, ws := newExecutorTools(t, config.ExecutorConfig{
		Python: config.ExecutorToolConfig{Enabled: true, Timeout: defaultTimeout, MaxOutputBytes: 4096},
	})
	require.NoError(t, os.WriteFile(filepath.Join(ws, "script.py"), []byte("print('from file')"), 0o644))

	reg := newRegistryWithExecutor(t, tools)
	res, err := reg.Call(executorCtx("u1"), executor.ToolPython, json.RawMessage(`{"file":"script.py"}`))
	require.NoError(t, err)
	m := res.(*executor.ExecutionResult)
	require.Equal(t, "from file\n", m.Output)
}

func TestExecutorSkillDirectoryAccess(t *testing.T) {
	if !executor.IsAvailable() {
		t.Skip("bwrap not available")
	}

	ws := t.TempDir()
	globalSkills := t.TempDir()
	tools := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(globalSkills, "marker.txt"), []byte("global\n"), 0o644))
	// Per-user skills live under the workspace at .blowball/skills and reach the
	// sandbox through the /workspace bind (no separate /skills/user mount).
	require.NoError(t, os.MkdirAll(filepath.Join(ws, ".blowball", "skills"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, ".blowball", "skills", "marker.txt"), []byte("user\n"), 0o644))

	toolsBundle := executor.NewTools(config.ExecutorConfig{
		Bash: config.ExecutorToolConfig{Enabled: true, Timeout: defaultTimeout, MaxOutputBytes: 4096},
	}, func(string) string { return ws }, globalSkills, tools)
	reg := newRegistryWithExecutor(t, toolsBundle)

	res, err := reg.Call(executorCtx("u1"), executor.ToolBash, json.RawMessage(`{"command":"cat /skills/global/marker.txt /workspace/.blowball/skills/marker.txt"}`))
	require.NoError(t, err)
	m := res.(*executor.ExecutionResult)
	require.Equal(t, "global\nuser\n", m.Output)

	// No separate per-user skills mount exists.
	res, err = reg.Call(executorCtx("u1"), executor.ToolBash, json.RawMessage(`{"command":"test -e /skills/user && echo exists || echo missing"}`))
	require.NoError(t, err)
	m = res.(*executor.ExecutionResult)
	require.Equal(t, "missing\n", m.Output, "/skills/user mount should no longer exist")

	res, err = reg.Call(executorCtx("u1"), executor.ToolBash, json.RawMessage(`{"command":"touch /skills/global/forbidden.txt 2>/dev/null; echo $?"}`))
	require.NoError(t, err)
	m = res.(*executor.ExecutionResult)
	require.Equal(t, "1\n", m.Output, "global skills directory should be read-only")
}

func TestExecutorWorkspaceIsolation(t *testing.T) {
	if !executor.IsAvailable() {
		t.Skip("bwrap not available")
	}

	tools, _ := newExecutorTools(t, config.ExecutorConfig{
		Bash: config.ExecutorToolConfig{Enabled: true, Timeout: defaultTimeout, MaxOutputBytes: 4096},
	})
	reg := newRegistryWithExecutor(t, tools)

	res, err := reg.Call(executorCtx("u1"), executor.ToolBash, json.RawMessage(`{"command":"cat /etc/shadow 2>&1 || true"}`))
	require.NoError(t, err)
	m := res.(*executor.ExecutionResult)
	require.NotContains(t, m.Output, "root:", "should not read /etc/shadow")

	res, err = reg.Call(executorCtx("u1"), executor.ToolBash, json.RawMessage(`{"command":"ls / | grep -v workspace | head -c 200 || true"}`))
	require.NoError(t, err)
	m = res.(*executor.ExecutionResult)
	require.Empty(t, m.Output, "should see only /workspace outside bind mounts")
}

func TestExecutorNetworkIsolation(t *testing.T) {
	if !executor.IsAvailable() {
		t.Skip("bwrap not available")
	}

	tools, _ := newExecutorTools(t, config.ExecutorConfig{
		Python: config.ExecutorToolConfig{Enabled: true, Timeout: defaultTimeout, MaxOutputBytes: 4096, Network: false},
	})
	reg := newRegistryWithExecutor(t, tools)

	res, err := reg.Call(executorCtx("u1"), executor.ToolPython, json.RawMessage(`{"code":"import socket; socket.create_connection(('127.0.0.1', 53), timeout=2)"}`))
	require.NoError(t, err)
	m := res.(*executor.ExecutionResult)
	require.NotEqual(t, 0, m.ExitCode, "network connection should fail")
}

func TestExecutorOutputTruncation(t *testing.T) {
	if !executor.IsAvailable() {
		t.Skip("bwrap not available")
	}

	tools, _ := newExecutorTools(t, config.ExecutorConfig{
		Bash: config.ExecutorToolConfig{Enabled: true, Timeout: defaultTimeout, MaxOutputBytes: 8},
	})
	reg := newRegistryWithExecutor(t, tools)

	res, err := reg.Call(executorCtx("u1"), executor.ToolBash, json.RawMessage(`{"command":"seq 1 100"}`))
	require.NoError(t, err)
	m := res.(*executor.ExecutionResult)
	require.True(t, m.Truncated)
	require.LessOrEqual(t, len(m.Output), 8+len("\n...output truncated..."))
}

func TestExecutorTimeout(t *testing.T) {
	if !executor.IsAvailable() {
		t.Skip("bwrap not available")
	}

	tools, _ := newExecutorTools(t, config.ExecutorConfig{
		Bash: config.ExecutorToolConfig{Enabled: true, Timeout: shortTimeout, MaxOutputBytes: 4096},
	})
	reg := newRegistryWithExecutor(t, tools)

	_, err := reg.Call(executorCtx("u1"), executor.ToolBash, json.RawMessage(`{"command":"sleep 10"}`))
	require.Error(t, err)
}

func TestExecutorPipInstallAndImport(t *testing.T) {
	if !executor.IsAvailable() {
		t.Skip("bwrap not available")
	}

	tools, _ := newExecutorTools(t, config.ExecutorConfig{
		Python: config.ExecutorToolConfig{Enabled: true, Timeout: defaultTimeout, MaxOutputBytes: 4096},
		Pip:    config.PipToolConfig{Enabled: true, Timeout: 120 * time.Second, MaxOutputBytes: 65536},
	})
	reg := newRegistryWithExecutor(t, tools)

	// Install a small, pure-Python package.
	res, err := reg.Call(executorCtx("u1"), executor.ToolPip, json.RawMessage(`{"packages":["colorama"],"upgrade":false}`))
	require.NoError(t, err)
	m := res.(*executor.ExecutionResult)
	require.Equal(t, 0, m.ExitCode, "pip install failed: %s", m.Output)

	// Verify it is importable from the python tool without sys.path changes.
	res, err = reg.Call(executorCtx("u1"), executor.ToolPython, json.RawMessage(`{"code":"import colorama; print(colorama.__version__)"}`))
	require.NoError(t, err)
	m = res.(*executor.ExecutionResult)
	require.Equal(t, 0, m.ExitCode, "import failed: %s", m.Output)
	require.NotEmpty(t, m.Output)
}

func TestExecutorPipInstallUsesMirror(t *testing.T) {
	if !executor.IsAvailable() {
		t.Skip("bwrap not available")
	}

	cfg := config.PipToolConfig{
		Enabled:        true,
		Timeout:        120 * time.Second,
		MaxOutputBytes: 65536,
		IndexURL:       "https://pypi.tuna.tsinghua.edu.cn/simple",
		TrustedHosts:   []string{"pypi.tuna.tsinghua.edu.cn"},
	}
	// Network is enabled by default; explicit false should make pip fail to reach
	// the configured mirror.
	f := false
	cfg.Network = &f

	tools, _ := newExecutorTools(t, config.ExecutorConfig{
		Pip: cfg,
	})
	reg := newRegistryWithExecutor(t, tools)

	res, err := reg.Call(executorCtx("u1"), executor.ToolPip, json.RawMessage(`{"packages":["colorama"]}`))
	require.NoError(t, err)
	m := res.(*executor.ExecutionResult)
	require.NotEqual(t, 0, m.ExitCode, "pip install should fail when network is disabled")
}

// TestExecutorSandboxHomeForced guards the executor-tools spec: HOME is forced to
// the synthetic writable path and resolves to a real mounted directory, so
// commands that cache/config under $HOME keep working.
func TestExecutorSandboxHomeForced(t *testing.T) {
	if !executor.IsAvailable() {
		t.Skip("bwrap not available")
	}

	tools, _ := newExecutorTools(t, config.ExecutorConfig{
		Bash: config.ExecutorToolConfig{Enabled: true, Timeout: defaultTimeout, MaxOutputBytes: 4096},
	})
	reg := newRegistryWithExecutor(t, tools)

	// HOME is forced to the synthetic path regardless of the host HOME.
	res, err := reg.Call(executorCtx("u1"), executor.ToolBash, json.RawMessage(`{"command":"echo $HOME"}`))
	require.NoError(t, err)
	m := res.(*executor.ExecutionResult)
	require.Equal(t, "/home/blowball\n", m.Output, "HOME should be forced to the synthetic sandbox path")

	// The synthetic home is a writable tmpfs, so writes under $HOME succeed.
	res, err = reg.Call(executorCtx("u1"), executor.ToolBash, json.RawMessage(`{"command":"echo hi > $HOME/.cache_foo && cat $HOME/.cache_foo"}`))
	require.NoError(t, err)
	m = res.(*executor.ExecutionResult)
	require.Equal(t, 0, m.ExitCode, "write to $HOME should succeed: %s", m.Output)
	require.Equal(t, "hi\n", m.Output)
}

// TestExecutorOperatorToolOnPath guards the executor-tools spec: the operator
// tools dir is mounted read-only at $HOME/.local/bin, prepended to PATH, and
// invocable by bare name.
func TestExecutorOperatorToolOnPath(t *testing.T) {
	if !executor.IsAvailable() {
		t.Skip("bwrap not available")
	}

	ws := t.TempDir()
	globalSkills := t.TempDir()
	toolsDir := t.TempDir()
	// Place an operator-provided executable in the tools dir.
	require.NoError(t, os.WriteFile(filepath.Join(toolsDir, "mytool"), []byte("#!/bin/sh\necho from-mytool\n"), 0o755))

	tools := executor.NewTools(config.ExecutorConfig{
		Bash: config.ExecutorToolConfig{Enabled: true, Timeout: defaultTimeout, MaxOutputBytes: 4096},
	}, func(string) string { return ws }, globalSkills, toolsDir)
	reg := newRegistryWithExecutor(t, tools)

	// $HOME/.local/bin is the first PATH entry.
	res, err := reg.Call(executorCtx("u1"), executor.ToolBash, json.RawMessage(`{"command":"echo $PATH | cut -d: -f1"}`))
	require.NoError(t, err)
	m := res.(*executor.ExecutionResult)
	require.Equal(t, "/home/blowball/.local/bin\n", m.Output, "first PATH entry should be the operator tools bin")

	// The operator tool resolves from $HOME/.local/bin via PATH.
	res, err = reg.Call(executorCtx("u1"), executor.ToolBash, json.RawMessage(`{"command":"mytool"}`))
	require.NoError(t, err)
	m = res.(*executor.ExecutionResult)
	require.Equal(t, 0, m.ExitCode, "mytool should resolve via PATH: %s", m.Output)
	require.Equal(t, "from-mytool\n", m.Output)

	// The tools dir is mounted read-only: creating a file under it must fail.
	res, err = reg.Call(executorCtx("u1"), executor.ToolBash, json.RawMessage(`{"command":"touch $HOME/.local/bin/evil 2>/dev/null; echo $?"}`))
	require.NoError(t, err)
	m = res.(*executor.ExecutionResult)
	require.NotEqual(t, "0\n", m.Output, "tools bin should be read-only")
}

const (
	defaultTimeout = 30 * time.Second
	shortTimeout   = 1 * time.Second
)
