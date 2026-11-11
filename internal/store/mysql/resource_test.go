package mysql

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

func TestResourceMetadataLifecycle(t *testing.T) {
	store, _, cleanup := setupScheduledStore(t)
	defer cleanup()
	ctx := context.Background()

	draft, err := store.EnsureResourceDraft(ctx, "user-1", "task-1")
	require.NoError(t, err)
	require.NotNil(t, draft)
	assert.Equal(t, uint64(0), draft.DraftRevision)

	again, err := store.EnsureResourceDraft(ctx, "user-1", "task-1")
	require.NoError(t, err)
	assert.Equal(t, draft.TaskID, again.TaskID)

	rev, err := store.BumpResourceDraft(ctx, "user-1", "task-1")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), rev)
	rev, err = store.BumpResourceDraft(ctx, "user-1", "task-1")
	require.NoError(t, err)
	assert.Equal(t, uint64(2), rev)

	next, err := store.NextResourceRevision(ctx, "task-1")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), next)

	v := resourceVersion("task-1", "user-1", 1, "req-1")
	inserted, err := store.InsertResourceVersion(ctx, v)
	require.NoError(t, err)
	assert.True(t, inserted)
	inserted, err = store.InsertResourceVersion(ctx, v)
	require.NoError(t, err)
	assert.False(t, inserted, "a duplicate request_id must not create a second version")

	found, err := store.FindResourceVersionByRequest(ctx, "user-1", "task-1", "req-1")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, uint64(1), found.Revision)

	versions, err := store.ListResourceVersions(ctx, "user-1", "task-1")
	require.NoError(t, err)
	require.Len(t, versions, 1)

	next, err = store.NextResourceRevision(ctx, "task-1")
	require.NoError(t, err)
	assert.Equal(t, uint64(2), next)

	deleted, err := store.DeleteResourceVersion(ctx, "user-1", "task-1", 1)
	require.NoError(t, err)
	assert.True(t, deleted)
	missing, err := store.GetResourceVersion(ctx, "user-1", "task-1", 1)
	require.NoError(t, err)
	assert.Nil(t, missing)

	// A foreign user cannot read the same task's version metadata.
	_, err = store.EnsureResourceDraft(ctx, "user-2", "task-1")
	require.NoError(t, err)
	foreign, err := store.GetResourceVersion(ctx, "user-2", "task-1", 2)
	require.NoError(t, err)
	assert.Nil(t, foreign)
}

func TestResourceLockSerializesBumps(t *testing.T) {
	store, _, cleanup := setupScheduledStore(t)
	defer cleanup()
	ctx := context.Background()
	_, err := store.EnsureResourceDraft(ctx, "user-1", "task-lock")
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = store.WithinTaskResourceLock(ctx, "task-lock", func() error {
				_, berr := store.BumpResourceDraft(ctx, "user-1", "task-lock")
				return berr
			})
		}()
	}
	wg.Wait()
	draft, err := store.GetResourceDraft(ctx, "user-1", "task-lock")
	require.NoError(t, err)
	assert.Equal(t, uint64(5), draft.DraftRevision)
}

func resourceVersion(taskID, userID string, revision uint64, requestID string) model.ScheduledResourceVersion {
	return model.ScheduledResourceVersion{
		TaskID: taskID, UserID: userID, Revision: revision,
		ManifestHash: "hash", FileCount: 1, TotalBytes: 3,
		ManifestJSON: `{"files":[]}`, RequestID: requestID,
	}
}
