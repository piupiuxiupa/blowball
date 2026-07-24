package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/tool/skill"
	"go.uber.org/zap"
)

// truncationMarker is appended to output that exceeded max_output_bytes.
const truncationMarker = "\n...output truncated..."

// dangerousCommandPattern detects common destructive or network-exfiltration
// commands. This is a warning-only check; execution is not blocked.
var dangerousCommandPattern = regexp.MustCompile(`\b(rm|curl|wget|sudo|sshd)\b`)

// ExecutionResult is the JSON-serializable return value for bash/python tools.
type ExecutionResult struct {
	Output    string `json:"output"`
	ExitCode  int    `json:"exit_code"`
	Truncated bool   `json:"truncated"`
}

// run executes the requested command inside a bwrap sandbox and returns its
// output, exit code, and truncation state.
func (t *Tools) run(ctx context.Context, toolName string, cfg config.ExecutorToolConfig, sandboxArgs []string) (*ExecutionResult, error) {
	workspaceRoot, err := t.userWorkspace(ctx)
	if err != nil {
		return nil, err
	}

	if err := requireAvailable(); err != nil {
		return nil, err
	}

	userSkillsDir, err := t.userSkillsDir(ctx)
	if err != nil {
		return nil, err
	}

	workspaceTmp := filepath.Join(workspaceRoot, "tmp")
	if err := os.MkdirAll(workspaceTmp, 0o755); err != nil {
		return nil, fmt.Errorf("executor: create workspace tmp: %w", err)
	}

	workspacePip := filepath.Join(workspaceRoot, ".pip")
	if err := os.MkdirAll(workspacePip, 0o755); err != nil {
		return nil, fmt.Errorf("executor: create workspace .pip: %w", err)
	}

	bwrapArgs := buildBwrapArgs(workspaceRoot, workspaceTmp, t.globalSkillsDir, userSkillsDir, t.toolsDir, t.cfg.Sandbox, cfg)
	bwrapArgs = append(bwrapArgs, sandboxArgs...)

	if toolName == ToolBash {
		logDangerousCommand(ctx, toolName, sandboxArgs)
	}

	start := time.Now()
	cmd := exec.CommandContext(ctx, "bwrap", bwrapArgs...)
	cmd.Stdin = nil

	w := &maxBytesWriter{max: cfg.MaxOutputBytes + 1024}
	cmd.Stdout = w
	cmd.Stderr = w

	runErr := cmd.Run()
	duration := time.Since(start)

	output := w.buf.Bytes()
	result := &ExecutionResult{ExitCode: 0}
	result.Output, result.Truncated = truncateOutput(output, cfg.MaxOutputBytes)

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			logAudit(ctx, toolName, sandboxArgs, -1, len(output), duration, runErr)
			return nil, fmt.Errorf("executor %s: %w", toolName, runErr)
		}
	}

	logAudit(ctx, toolName, sandboxArgs, result.ExitCode, len(output), duration, nil)
	return result, nil
}

// maxBytesWriter is a concurrency-safe writer used for both stdout and stderr.
// It keeps up to max bytes and silently discards the rest so the process never
// blocks on a full pipe.
type maxBytesWriter struct {
	buf bytes.Buffer
	max int
}

func (w *maxBytesWriter) Write(p []byte) (int, error) {
	if w.max <= 0 {
		return w.buf.Write(p)
	}
	if w.buf.Len() >= w.max {
		return len(p), nil
	}
	space := w.max - w.buf.Len()
	if len(p) > space {
		w.buf.Write(p[:space])
		return len(p), nil
	}
	return w.buf.Write(p)
}

// logAudit emits a structured audit entry for every command execution.
func logAudit(ctx context.Context, toolName string, sandboxArgs []string, exitCode, outputBytes int, duration time.Duration, err error) {
	userID := skill.UserIDFromContext(ctx)
	fields := []zap.Field{
		zap.String("tool", toolName),
		zap.String("user_id", userID),
		zap.String("command", formatCommand(sandboxArgs)),
		zap.Int("exit_code", exitCode),
		zap.Int("output_bytes", outputBytes),
		zap.Duration("duration", duration),
	}
	if err != nil {
		fields = append(fields, zap.Error(err))
		logger.L().Error("executor audit", fields...)
		return
	}
	logger.L().Info("executor audit", fields...)
}

// logDangerousCommand emits a warning when the command matches a known
// dangerous pattern. Execution is not blocked.
func logDangerousCommand(ctx context.Context, toolName string, sandboxArgs []string) {
	if detectDangerousCommand(formatCommand(sandboxArgs)) {
		logger.L().Warn("executor dangerous command detected",
			zap.String("tool", toolName),
			zap.String("user_id", skill.UserIDFromContext(ctx)),
			zap.String("command", formatCommand(sandboxArgs)),
		)
	}
}

// detectDangerousCommand reports whether cmd contains a dangerous keyword.
func detectDangerousCommand(cmd string) bool {
	return dangerousCommandPattern.MatchString(cmd)
}

// truncateOutput truncates output to max bytes, appending a marker when
// truncation occurs. It returns the (possibly truncated) string and a flag.
func truncateOutput(output []byte, max int) (string, bool) {
	if max <= 0 || len(output) <= max {
		return string(output), false
	}
	out := append(output[:max:max], []byte(truncationMarker)...)
	return string(out), true
}

// formatCommand reconstructs a human-readable command string from the sandbox
// arguments (everything after the bwrap options, i.e. the real command).
func formatCommand(sandboxArgs []string) string {
	return strings.Join(sandboxArgs, " ")
}
