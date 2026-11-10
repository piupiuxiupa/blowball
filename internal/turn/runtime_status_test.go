package turn

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
)

func runtimeStatusReplayFixture() ([]agent.Message, []int, []model.Message) {
	messages := []agent.Message{
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "calling", ToolCalls: []agent.ToolCall{{ID: "call-1"}}},
		{Role: "tool", ToolCallID: "call-1", Content: "result"},
		{Role: "user", Content: "new question"},
	}
	lastRow := []int{0, 1, 2, 3}
	rows := []model.Message{
		{Role: model.RoleUser, ClientMsgID: "trace-1:0"},
		{Role: model.RoleAssistant},
		{Role: model.RoleAssistant, ToolCallID: "call-1"},
		{Role: model.RoleUser, ClientMsgID: "trace-2:0"},
	}
	return messages, lastRow, rows
}

func TestReplayRuntimeStatus_ReplaysAtOriginalAnchors(t *testing.T) {
	messages, lastRow, rows := runtimeStatusReplayFixture()
	original := append([]agent.Message(nil), messages...)
	entries := []model.RuntimeStatus{
		{StatusID: "s-1", Kind: agent.RuntimeStatusCurrent, Content: "old current", Seq: 1, BeforeClientMsgID: "trace-1:0"},
		{StatusID: "s-2", Kind: agent.RuntimeStatusToolBudget, Content: "round one", Seq: 2, AfterToolCallID: "call-1"},
		{StatusID: "s-3", Kind: agent.RuntimeStatusCurrent, Content: "new current", Seq: 3, BeforeClientMsgID: "trace-2:0"},
	}

	got, ok := replayRuntimeStatus(messages, lastRow, rows, entries)
	require.True(t, ok)
	require.Len(t, got, 7)
	assert.Equal(t, "old current", got[0].Content)
	assert.Equal(t, agent.RuntimeStatusCurrent, got[0].RuntimeStatus)
	assert.Equal(t, "old question", got[1].Content)
	assert.Equal(t, "round one", got[4].Content)
	assert.Equal(t, agent.RuntimeStatusToolBudget, got[4].RuntimeStatus)
	assert.Equal(t, "new current", got[5].Content)
	assert.Equal(t, "new question", got[6].Content)
	assert.Equal(t, original, messages, "canonical input must not be mutated")
}

func TestReplayRuntimeStatus_RejectsWholeLogForAnyInvalidEntry(t *testing.T) {
	valid := model.RuntimeStatus{
		StatusID: "valid", Kind: agent.RuntimeStatusCurrent,
		Content: "status", Seq: 1, BeforeClientMsgID: "trace-1:0",
	}
	tests := []struct {
		name    string
		entries []model.RuntimeStatus
	}{
		{name: "missing anchor", entries: []model.RuntimeStatus{valid, {
			StatusID: "missing", Kind: agent.RuntimeStatusCurrent,
			Content: "missing", Seq: 2, BeforeClientMsgID: "unknown",
		}}},
		{name: "tool result missing after crash", entries: []model.RuntimeStatus{valid, {
			StatusID: "orphan-tool", Kind: agent.RuntimeStatusToolBudget,
			Content: "orphan", Seq: 2, AfterToolCallID: "not-persisted",
		}}},
		{name: "missing status id", entries: []model.RuntimeStatus{{
			Kind: agent.RuntimeStatusCurrent, Content: "status", Seq: 1, BeforeClientMsgID: "trace-1:0",
		}}},
		{name: "unknown kind", entries: []model.RuntimeStatus{{
			StatusID: "kind", Kind: "unknown", Content: "status", Seq: 1, BeforeClientMsgID: "trace-1:0",
		}}},
		{name: "empty content", entries: []model.RuntimeStatus{{
			StatusID: "empty", Kind: agent.RuntimeStatusCurrent, Seq: 1, BeforeClientMsgID: "trace-1:0",
		}}},
		{name: "both anchors", entries: []model.RuntimeStatus{{
			StatusID: "both", Kind: agent.RuntimeStatusCurrent, Content: "status", Seq: 1,
			BeforeClientMsgID: "trace-1:0", AfterToolCallID: "call-1",
		}}},
		{name: "no anchor", entries: []model.RuntimeStatus{{
			StatusID: "none", Kind: agent.RuntimeStatusCurrent, Content: "status", Seq: 1,
		}}},
		{name: "non increasing sequence", entries: []model.RuntimeStatus{valid, {
			StatusID: "backward", Kind: agent.RuntimeStatusCurrent,
			Content: "backward", Seq: 1, BeforeClientMsgID: "trace-2:0",
		}}},
		{name: "duplicate status id", entries: []model.RuntimeStatus{valid, valid}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages, lastRow, rows := runtimeStatusReplayFixture()
			got, ok := replayRuntimeStatus(messages, lastRow, rows, tt.entries)
			assert.False(t, ok)
			assert.Nil(t, got)
		})
	}
}

func TestReplayRuntimeStatus_EmptyLogKeepsCanonicalMessages(t *testing.T) {
	messages, lastRow, rows := runtimeStatusReplayFixture()
	got, ok := replayRuntimeStatus(messages, lastRow, rows, nil)
	require.True(t, ok)
	assert.Equal(t, messages, got)
}

type fakeRuntimeStatusStore struct {
	entries     []model.RuntimeStatus
	listErr     error
	clearCalls  int
	appendCalls int
}

func (f *fakeRuntimeStatusStore) AppendRuntimeStatus(_ context.Context, _ string, status model.RuntimeStatus) error {
	f.appendCalls++
	f.entries = append(f.entries, status)
	return nil
}

func (f *fakeRuntimeStatusStore) ListRuntimeStatus(_ context.Context, _ string) ([]model.RuntimeStatus, error) {
	return f.entries, f.listErr
}

func (f *fakeRuntimeStatusStore) ClearRuntimeStatus(_ context.Context, _ string) error {
	f.clearCalls++
	f.entries = nil
	return nil
}

func TestExecutorReplaySessionRuntimeStatus_InvalidLogStartsNewEpoch(t *testing.T) {
	messages, lastRow, rows := runtimeStatusReplayFixture()
	store := &fakeRuntimeStatusStore{entries: []model.RuntimeStatus{{
		StatusID: "orphan", Kind: agent.RuntimeStatusCurrent,
		Content: "orphan", Seq: 1, BeforeClientMsgID: "missing",
	}}}
	executor := &Executor{runtimeStatus: store}

	got := executor.replaySessionRuntimeStatus(context.Background(), "s-1", messages, lastRow, rows)
	assert.Equal(t, messages, got)
	assert.Equal(t, 1, store.clearCalls)
	assert.Empty(t, store.entries)
}

func TestExecutorReplaySessionRuntimeStatus_ReadFailureStartsNewEpoch(t *testing.T) {
	messages, lastRow, rows := runtimeStatusReplayFixture()
	store := &fakeRuntimeStatusStore{listErr: errors.New("redis down")}
	executor := &Executor{runtimeStatus: store}

	got := executor.replaySessionRuntimeStatus(context.Background(), "s-1", messages, lastRow, rows)
	assert.Equal(t, messages, got)
	assert.Equal(t, 1, store.clearCalls)
}

func TestExecutorReplaySessionRuntimeStatus_NilStoreUsesCanonicalMessages(t *testing.T) {
	messages, lastRow, rows := runtimeStatusReplayFixture()
	executor := &Executor{}
	got := executor.replaySessionRuntimeStatus(context.Background(), "s-1", messages, lastRow, rows)
	assert.Equal(t, messages, got)
}
