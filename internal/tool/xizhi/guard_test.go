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
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/resource"
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
	assert.Equal(t, "user-1:.cron/t/draft/a.txt:modify", g.calls[0])
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

type draftGuardStore struct {
	resource.Store
	userID   string
	taskID   string
	revision uint64
	bumpErr  error
}

func (s *draftGuardStore) OwnsTask(_ context.Context, userID, taskID string) (bool, error) {
	return userID == s.userID && taskID == s.taskID, nil
}

func (s *draftGuardStore) EnsureResourceDraft(_ context.Context, userID, taskID string) (*model.ScheduledResourceDraft, error) {
	return &model.ScheduledResourceDraft{UserID: userID, TaskID: taskID, DraftRevision: s.revision}, nil
}

func (s *draftGuardStore) BumpResourceDraft(_ context.Context, _, _ string) (uint64, error) {
	if s.bumpErr != nil {
		return 0, s.bumpErr
	}
	s.revision++
	return s.revision, nil
}

func TestWriteFileResourceGuardContract(t *testing.T) {
	const taskID = "11111111-1111-4111-8111-111111111111"
	draftPath := ".cron/" + taskID + "/draft/a.txt"
	revisionPath := ".cron/" + taskID + "/revisions/1/a.txt"
	cases := []struct {
		name         string
		path         string
		existing     bool
		owner        string
		bumpErr      error
		wantErr      error
		wantRevision uint64
	}{
		{name: "owned draft create", path: draftPath, owner: "user-1", wantRevision: 1},
		{name: "owned draft existing", path: draftPath, existing: true, owner: "user-1", wantRevision: 1},
		{name: "ordinary create", path: "a.txt", owner: "user-1"},
		{name: "ordinary existing", path: "a.txt", existing: true, owner: "user-1"},
		{name: "protected create", path: revisionPath, owner: "user-1", wantErr: resource.ErrReadOnly},
		{name: "protected existing", path: revisionPath, existing: true, owner: "user-1", wantErr: resource.ErrReadOnly},
		{name: "other owner", path: draftPath, existing: true, owner: "user-2", wantErr: resource.ErrNotFound},
		{name: "fence unavailable", path: draftPath, existing: true, owner: "user-1", bumpErr: errors.New("bump failed"), wantErr: resource.ErrUnavailable},
	}
	modes := []struct {
		name  string
		value string
	}{
		{name: "default"},
		{name: "write", value: "write"},
		{name: "append", value: "append"},
	}
	for _, mode := range modes {
		for _, tc := range cases {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				ws := t.TempDir()
				_, err := resource.EnsureLayout(t.Context(), ws, "user-1", taskID)
				require.NoError(t, err)
				absPath := filepath.Join(ws, tc.path)
				if tc.existing {
					require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o755))
					require.NoError(t, os.WriteFile(absPath, []byte("old"), 0o644))
				}
				store := &draftGuardStore{userID: tc.owner, taskID: taskID, bumpErr: tc.bumpErr}
				guard := resource.NewGuard(resource.New(store, store, func(string) string { return ws }, resource.Limits{}))
				reg := tool.NewRegistry()
				RegisterAll(reg, ws, config.XizhiConfig{}, nil, guard)
				write, ok := reg.Get(NameWriteFile)
				require.True(t, ok)
				args, err := json.Marshal(writeArgs{Path: tc.path, Content: "new", Mode: mode.value})
				require.NoError(t, err)

				_, err = write.Execute(skill.WithUserID(t.Context(), "user-1"), args)
				wantContent := "new"
				if tc.wantErr != nil {
					require.ErrorIs(t, err, tc.wantErr)
					wantContent = "old"
				} else {
					require.NoError(t, err)
					if mode.value == "append" && tc.existing {
						wantContent = "oldnew"
					}
				}
				assert.Equal(t, tc.wantRevision, store.revision)
				if tc.wantErr != nil && !tc.existing {
					_, statErr := os.Stat(absPath)
					assert.True(t, os.IsNotExist(statErr))
					return
				}
				content, readErr := os.ReadFile(absPath)
				require.NoError(t, readErr)
				assert.Equal(t, wantContent, string(content))
			})
		}
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
