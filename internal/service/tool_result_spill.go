package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/tool/xizhi"
	"go.uber.org/zap"
)

const toolResultSpillDirRel = "tmp/compaction-outputs"

// toolResultSpillRelPath returns the deterministic workspace-relative path
// used for a truncated retained result. The hash is derived only from stable
// tool identity, so replaying the same summary never churns the path.
func toolResultSpillRelPath(sessionID string, call agent.ToolCall) string {
	sum := sha256.Sum256([]byte(call.ID + "\x00" + call.Function.Name))
	name := hex.EncodeToString(sum[:12]) + ".txt"
	return filepath.ToSlash(filepath.Join(
		toolResultSpillDirRel,
		sanitizeToolResultSegment(sessionID),
		name,
	))
}

// spillRetainedToolResult materializes the full persisted result inside the
// user's workspace. The workspace-relative path is readable with the ordinary
// xizhi read/grep tools; a write failure returns no reference and an explicit
// fallback note instead of a fake retrieval path.
func (s *CompactionService) spillRetainedToolResult(
	ctx context.Context, userID, sessionID string, call agent.ToolCall, full string,
) (string, string) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Sprintf("full result remains in persisted tool_calls; spill cancelled: %v", err)
	}
	if s == nil || s.deps.FS == nil {
		return "", "full result remains in persisted tool_calls; workspace spill unavailable"
	}
	workspace := strings.TrimSpace(s.deps.FS.UserWorkspace(userID))
	if workspace == "" {
		return "", "full result remains in persisted tool_calls; workspace path unavailable"
	}
	relPath := toolResultSpillRelPath(sessionID, call)
	if _, err := xizhi.ValidatePath(workspace, relPath); err != nil {
		logger.FromContext(ctx).Warn("tool result spill path rejected",
			zap.String("session_id", sessionID),
			zap.String("path", relPath),
			zap.Error(err))
		return "", "full result remains in persisted tool_calls; workspace spill path rejected"
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		logger.FromContext(ctx).Warn("tool result spill root open failed",
			zap.String("session_id", sessionID),
			zap.Error(err))
		return "", "full result remains in persisted tool_calls; workspace spill unavailable"
	}
	defer root.Close()
	rootRelPath := filepath.FromSlash(relPath)
	if err := root.MkdirAll(filepath.Dir(rootRelPath), 0o755); err != nil {
		logger.FromContext(ctx).Warn("tool result spill mkdir failed",
			zap.String("session_id", sessionID),
			zap.String("path", relPath),
			zap.Error(err))
		return "", "full result remains in persisted tool_calls; workspace spill failed"
	}
	// Revalidate after directory creation to close the validation-to-create
	// window. OpenRoot then confines every remaining lookup to this workspace
	// even if a symlink is swapped concurrently.
	if _, err := xizhi.ValidatePath(workspace, relPath); err != nil {
		logger.FromContext(ctx).Warn("tool result spill path rejected after mkdir",
			zap.String("session_id", sessionID),
			zap.String("path", relPath),
			zap.Error(err))
		return "", "full result remains in persisted tool_calls; workspace spill path rejected"
	}
	if info, err := root.Lstat(rootRelPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", "full result remains in persisted tool_calls; workspace spill path is a symlink"
		}
	} else if !os.IsNotExist(err) {
		logger.FromContext(ctx).Warn("tool result spill stat failed",
			zap.String("session_id", sessionID),
			zap.String("path", relPath),
			zap.Error(err))
		return "", "full result remains in persisted tool_calls; workspace spill failed"
	}
	if err := root.WriteFile(rootRelPath, []byte(full), 0o644); err != nil {
		logger.FromContext(ctx).Warn("tool result spill write failed",
			zap.String("session_id", sessionID),
			zap.String("path", relPath),
			zap.Error(err))
		return "", "full result remains in persisted tool_calls; workspace spill failed"
	}
	return relPath, ""
}

func sanitizeToolResultSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "session"
	}
	return out
}
