package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lush/blowball/internal/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpillRetainedToolResult_RejectsSymlinkEscapes(t *testing.T) {
	call := agent.ToolCall{
		ID:       "call-symlink",
		Function: agent.ToolCallFunction{Name: "executor_bash"},
	}
	relPath := toolResultSpillRelPath("session-1", call)

	t.Run("parent directory", func(t *testing.T) {
		workspace := t.TempDir()
		externalDir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(workspace, "tmp"), 0o755))
		require.NoError(t, os.Symlink(externalDir, filepath.Join(workspace, "tmp", "compaction-outputs")))
		svc := &CompactionService{deps: SessionDeps{FS: &fakeFSStore{workspace: workspace}}}

		ref, note := svc.spillRetainedToolResult(context.Background(), "user-1", "session-1", call, "secret")

		assert.Empty(t, ref)
		assert.Contains(t, note, "workspace spill")
		entries, err := os.ReadDir(externalDir)
		require.NoError(t, err)
		assert.Empty(t, entries, "spill must not create anything through an external parent symlink")
	})

	t.Run("final file", func(t *testing.T) {
		workspace := t.TempDir()
		externalDir := t.TempDir()
		externalFile := filepath.Join(externalDir, "target.txt")
		require.NoError(t, os.WriteFile(externalFile, []byte("unchanged"), 0o644))
		absPath := filepath.Join(workspace, filepath.FromSlash(relPath))
		require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o755))
		require.NoError(t, os.Symlink(externalFile, absPath))
		svc := &CompactionService{deps: SessionDeps{FS: &fakeFSStore{workspace: workspace}}}

		ref, note := svc.spillRetainedToolResult(context.Background(), "user-1", "session-1", call, "secret")

		assert.Empty(t, ref)
		assert.Contains(t, note, "workspace spill")
		data, err := os.ReadFile(externalFile)
		require.NoError(t, err)
		assert.Equal(t, "unchanged", string(data), "spill must not overwrite a final-component symlink target")
	})
}
