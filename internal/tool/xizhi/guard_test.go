package xizhi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/skill"
)

type recordingGuard struct {
	calls []string
	deny  bool
	err   error
}

func (g *recordingGuard) CheckWrite(_ context.Context, userID, relPath, kind string) error {
	g.calls = append(g.calls, userID+":"+relPath+":"+kind)
	if g.err != nil {
		return g.err
	}
	if g.deny {
		return errors.New("cron resource path is read-only")
	}
	return nil
}

func guardRegistry(t *testing.T, g PathGuard) (*tool.Registry, string) {
	t.Helper()
	ws := t.TempDir()
	reg := tool.NewRegistry()
	RegisterAll(reg, ws, config.XizhiConfig{}, nil, g)
	return reg, ws
}

func TestFileToolsCallTheResourceGuard(t *testing.T) {
	g := &recordingGuard{}
	reg, ws := guardRegistry(t, g)
	ctx := skill.WithUserID(context.Background(), "user-1")

	write, _ := reg.Get(NameWriteFile)
	_, err := write.Execute(ctx, json.RawMessage(`{"path":".cron/t/draft/a.txt","content":"x"}`))
	require.NoError(t, err)
	require.Len(t, g.calls, 1)
	assert.Equal(t, "user-1:.cron/t/draft/a.txt:write", g.calls[0])
	_, statErr := os.Stat(filepath.Join(ws, ".cron", "t", "draft", "a.txt"))
	require.NoError(t, statErr)

	modify, _ := reg.Get(NameModifyFile)
	_, err = modify.Execute(ctx, json.RawMessage(`{"path":".cron/t/draft/a.txt","old_content":"x","new_content":"y"}`))
	require.NoError(t, err)
	assert.Equal(t, "user-1:.cron/t/draft/a.txt:modify", g.calls[1])

	del, ok := reg.Get(NameDeleteFile)
	if ok {
		_, err = del.Execute(ctx, json.RawMessage(`{"path":".cron/t/draft/a.txt"}`))
		require.NoError(t, err)
		assert.Equal(t, "user-1:.cron/t/draft/a.txt:delete", g.calls[2])
	}
}

func TestGuardedWriteIsRejectedBeforeTouchingDisk(t *testing.T) {
	g := &recordingGuard{deny: true}
	reg, ws := guardRegistry(t, g)
	write, _ := reg.Get(NameWriteFile)
	_, err := write.Execute(skill.WithUserID(context.Background(), "user-1"),
		json.RawMessage(`{"path":".cron/t/revisions/1/a.txt","content":"x"}`))
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(ws, ".cron", "t", "revisions", "1", "a.txt"))
	assert.True(t, os.IsNotExist(statErr))
}

func TestReadsDoNotCallTheGuard(t *testing.T) {
	g := &recordingGuard{}
	ws := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hello"), 0o644))
	reg := tool.NewRegistry()
	RegisterAll(reg, ws, config.XizhiConfig{}, nil, g)
	read, _ := reg.Get(NameReadFile)
	_, err := read.Execute(skill.WithUserID(context.Background(), "user-1"), json.RawMessage(`{"path":"a.txt"}`))
	require.NoError(t, err)
	assert.Empty(t, g.calls)
}
