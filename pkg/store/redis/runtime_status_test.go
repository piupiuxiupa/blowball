package redis

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/agent"
	"github.com/lush/blowball/pkg/model"
)

func TestRuntimeStatus_AppendListIsIdempotentAndTTLBound(t *testing.T) {
	store, mr := newTestStore(t, time.Minute)
	ctx := context.Background()
	first := model.RuntimeStatus{
		StatusID: "trace-1:0:current", Kind: agent.RuntimeStatusCurrent,
		Content: "original", Seq: 1, BeforeClientMsgID: "trace-1:0",
	}
	second := model.RuntimeStatus{
		StatusID: "trace-1:1:tool", Kind: agent.RuntimeStatusToolBudget,
		Content: "budget", Seq: 2, AfterToolCallID: "call-1",
	}
	require.NoError(t, store.AppendRuntimeStatus(ctx, "s-1", first))
	require.NoError(t, store.AppendRuntimeStatus(ctx, "s-1", second))

	duplicate := first
	duplicate.Content = "must not replace the immutable original"
	require.NoError(t, store.AppendRuntimeStatus(ctx, "s-1", duplicate))

	got, err := store.ListRuntimeStatus(ctx, "s-1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "original", got[0].Content)
	assert.Equal(t, second, got[1])
	assert.Positive(t, mr.TTL("runtime_status:s-1"))

	mr.FastForward(2 * time.Minute)
	got, err = store.ListRuntimeStatus(ctx, "s-1")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestDelSessionCache_ClearsRuntimeStatus(t *testing.T) {
	store, _ := newTestStore(t, time.Hour)
	ctx := context.Background()
	entry := model.RuntimeStatus{
		StatusID: "trace-1:0:current", Kind: agent.RuntimeStatusCurrent,
		Content: "status", Seq: 1, BeforeClientMsgID: "trace-1:0",
	}
	require.NoError(t, store.AppendRuntimeStatus(ctx, "s-1", entry))
	require.NoError(t, store.DelSessionCache(ctx, "s-1"))

	got, err := store.ListRuntimeStatus(ctx, "s-1")
	require.NoError(t, err)
	assert.Empty(t, got)
	require.NoError(t, store.AppendRuntimeStatus(ctx, "s-1", entry),
		"the id registry must be cleared with the status entries")
}

func TestRuntimeStatus_ClearDeletesEntriesAndIDRegistry(t *testing.T) {
	store, _ := newTestStore(t, time.Hour)
	ctx := context.Background()
	entry := model.RuntimeStatus{
		StatusID: "trace-1:0:current", Kind: agent.RuntimeStatusCurrent,
		Content: "status", Seq: 1, BeforeClientMsgID: "trace-1:0",
	}
	require.NoError(t, store.AppendRuntimeStatus(ctx, "s-1", entry))
	require.NoError(t, store.ClearRuntimeStatus(ctx, "s-1"))

	got, err := store.ListRuntimeStatus(ctx, "s-1")
	require.NoError(t, err)
	assert.Empty(t, got)

	require.NoError(t, store.AppendRuntimeStatus(ctx, "s-1", entry))
	got, err = store.ListRuntimeStatus(ctx, "s-1")
	require.NoError(t, err)
	assert.Equal(t, []model.RuntimeStatus{entry}, got)
}
