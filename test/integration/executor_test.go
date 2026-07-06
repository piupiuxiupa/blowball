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
	return executor.NewTools(cfg, func(userID string) string {
		return ws
	}), ws
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

const (
	defaultTimeout = 30 * time.Second
	shortTimeout   = 1 * time.Second
)
