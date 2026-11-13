package turn

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/agent"
	"github.com/lush/blowball/pkg/model"
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

func TestReplayRuntimeStatus_ToolTailRecallBoundary(t *testing.T) {
	rows, calls, canonical := toolTailRecallFixture()
	messages, lastRow, err := MessagesToAgentMessagesIndexed(rows, calls)
	require.NoError(t, err)
	before, err := json.Marshal(messages)
	require.NoError(t, err)
	statuses := []model.RuntimeStatus{
		{StatusID: "current-a", Kind: agent.RuntimeStatusCurrent, Content: "<current_status>A</current_status>", Seq: 1, BeforeClientMsgID: "turn-a:0"},
		{StatusID: "budget-a", Kind: agent.RuntimeStatusToolBudget, Content: "<tool_budget>one</tool_budget>", Seq: 2, AfterToolCallID: "call-a"},
		{StatusID: "current-b", Kind: agent.RuntimeStatusCurrent, Content: "<current_status>B</current_status>", Seq: 3, BeforeClientMsgID: "turn-b:0"},
	}
	want := []agent.Message{
		{Role: "user", Content: "<current_status>A</current_status>", RuntimeStatus: agent.RuntimeStatusCurrent, RuntimeStatusSeq: 1},
		canonical[0], canonical[1], canonical[2],
		{Role: "user", Content: "<tool_budget>one</tool_budget>", RuntimeStatus: agent.RuntimeStatusToolBudget, RuntimeStatusSeq: 2},
		canonical[3], canonical[4],
		{Role: "user", Content: "<current_status>B</current_status>", RuntimeStatus: agent.RuntimeStatusCurrent, RuntimeStatusSeq: 3},
		canonical[5],
	}

	got, ok := replayRuntimeStatus(messages, lastRow, rows, statuses)
	require.True(t, ok)
	assert.Equal(t, want, got)
	requireCompleteToolGroups(t, got)
	require.GreaterOrEqual(t, len(got), 5)
	previousRequest, err := json.Marshal(want[:5])
	require.NoError(t, err)
	replayedPrefix, err := json.Marshal(got[:5])
	require.NoError(t, err)
	assert.Equal(t, string(previousRequest), string(replayedPrefix))
	after, err := json.Marshal(messages)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "replay must not mutate canonical messages")
}

func requireCompleteToolGroups(t *testing.T, messages []agent.Message) {
	t.Helper()
	pending := make(map[string]struct{})
	for i, message := range messages {
		if len(pending) > 0 {
			require.Equal(t, "tool", message.Role, "messages[%d] splits an unfinished tool group", i)
			require.Contains(t, pending, message.ToolCallID, "messages[%d] has no matching call", i)
			delete(pending, message.ToolCallID)
			continue
		}
		require.NotEqual(t, "tool", message.Role, "messages[%d] is an orphan result", i)
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				pending[call.ID] = struct{}{}
			}
		}
	}
	require.Empty(t, pending)
}

func parallelRuntimeStatusFixture() ([]agent.Message, []int, []model.Message) {
	messages, _, rows := runtimeStatusReplayFixture()
	messages[1].ToolCalls = []agent.ToolCall{{ID: "call-1"}, {ID: "call-2"}, {ID: "call-3"}}
	messages = []agent.Message{
		messages[0], messages[1], messages[2],
		{Role: "tool", ToolCallID: "call-2", Content: "second"},
		{Role: "tool", ToolCallID: "call-3", Content: "third"},
		messages[3],
	}
	rows = []model.Message{
		rows[0], rows[1], rows[2],
		{Role: model.RoleAssistant, ToolCallID: "call-2"},
		{Role: model.RoleAssistant, ToolCallID: "call-3"},
		rows[3],
	}
	return messages, []int{0, 1, 2, 3, 4, 5}, rows
}

func TestReplayRuntimeStatus_RejectsInvalidProtocolAnchors(t *testing.T) {
	tests := []struct {
		name     string
		statuses []model.RuntimeStatus
		mutate   func([]agent.Message, []model.Message, []model.RuntimeStatus)
	}{
		{name: "middle result", mutate: func(_ []agent.Message, _ []model.Message, entries []model.RuntimeStatus) {
			entries[0].AfterToolCallID = "call-2"
		}},
		{name: "unfinished group", mutate: func(messages []agent.Message, _ []model.Message, entries []model.RuntimeStatus) {
			messages[4] = agent.Message{Role: "user", Content: "interruption"}
			entries[0].AfterToolCallID = "call-2"
		}},
		{name: "mismatched result", mutate: func(messages []agent.Message, _ []model.Message, _ []model.RuntimeStatus) {
			messages[3].ToolCallID = "unknown"
		}},
		{name: "orphan result", mutate: func(messages []agent.Message, _ []model.Message, _ []model.RuntimeStatus) {
			messages[1].ToolCalls = nil
		}},
		{name: "duplicate call id", mutate: func(messages []agent.Message, _ []model.Message, _ []model.RuntimeStatus) {
			messages[1].ToolCalls[1].ID = "call-1"
		}},
		{name: "empty call id", mutate: func(messages []agent.Message, _ []model.Message, _ []model.RuntimeStatus) {
			messages[1].ToolCalls[0].ID = ""
		}},
		{name: "before assistant", mutate: func(_ []agent.Message, rows []model.Message, entries []model.RuntimeStatus) {
			rows[1].ClientMsgID = "not-user"
			entries[0].Kind, entries[0].AfterToolCallID, entries[0].BeforeClientMsgID = agent.RuntimeStatusCurrent, "", "not-user"
		}},
		{name: "before tool result", mutate: func(_ []agent.Message, rows []model.Message, entries []model.RuntimeStatus) {
			rows[2].ClientMsgID = "not-user"
			entries[0].Kind, entries[0].AfterToolCallID, entries[0].BeforeClientMsgID = agent.RuntimeStatusCurrent, "", "not-user"
		}},
		{name: "user message with non-user source", mutate: func(_ []agent.Message, rows []model.Message, entries []model.RuntimeStatus) {
			rows[0].Role = model.RoleAssistant
			entries[0].Kind, entries[0].AfterToolCallID, entries[0].BeforeClientMsgID = agent.RuntimeStatusCurrent, "", "trace-1:0"
		}},
		{name: "current status with after-tool anchor", mutate: func(_ []agent.Message, _ []model.Message, entries []model.RuntimeStatus) {
			entries[0].Kind = agent.RuntimeStatusCurrent
		}},
		{name: "tool budget with before-user anchor", mutate: func(_ []agent.Message, _ []model.Message, entries []model.RuntimeStatus) {
			entries[0].AfterToolCallID, entries[0].BeforeClientMsgID = "", "trace-1:0"
		}},
		{name: "reverse replay sequence", statuses: []model.RuntimeStatus{
			{StatusID: "current", Kind: agent.RuntimeStatusCurrent, Content: "current", Seq: 1, BeforeClientMsgID: "trace-2:0"},
			{StatusID: "budget", Kind: agent.RuntimeStatusToolBudget, Content: "budget", Seq: 2, AfterToolCallID: "call-3"},
		}},
		{name: "duplicate after anchor masks missing entry", statuses: []model.RuntimeStatus{
			{StatusID: "budget", Kind: agent.RuntimeStatusToolBudget, Content: "budget", Seq: 1, AfterToolCallID: "call-3"},
			{StatusID: "missing", Kind: agent.RuntimeStatusCurrent, Content: "missing", Seq: 2, BeforeClientMsgID: "missing-user"},
		}, mutate: func(messages []agent.Message, _ []model.Message, _ []model.RuntimeStatus) {
			messages[3].ToolCallID = "call-3"
		}},
		{name: "duplicate before anchor masks missing entry", statuses: []model.RuntimeStatus{
			{StatusID: "current", Kind: agent.RuntimeStatusCurrent, Content: "current", Seq: 1, BeforeClientMsgID: "trace-1:0"},
			{StatusID: "missing", Kind: agent.RuntimeStatusToolBudget, Content: "missing", Seq: 2, AfterToolCallID: "missing-call"},
		}, mutate: func(_ []agent.Message, rows []model.Message, _ []model.RuntimeStatus) {
			rows[5].ClientMsgID = "trace-1:0"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages, lastRow, rows := parallelRuntimeStatusFixture()
			entries := tt.statuses
			if entries == nil {
				entries = []model.RuntimeStatus{{
					StatusID: "budget", Kind: agent.RuntimeStatusToolBudget, Content: "budget",
					Seq: 1, AfterToolCallID: "call-3",
				}}
			}
			if tt.mutate != nil {
				tt.mutate(messages, rows, entries)
			}

			got, ok := replayRuntimeStatus(messages, lastRow, rows, entries)
			assert.False(t, ok)
			assert.Nil(t, got, "invalid replay must not return a partial transcript")
		})
	}
}

func TestReplayRuntimeStatus_ParallelGroupPreservesBytesAndSequenceGaps(t *testing.T) {
	messages, lastRow, rows := parallelRuntimeStatusFixture()
	before, err := json.Marshal(messages)
	require.NoError(t, err)
	entries := []model.RuntimeStatus{
		{StatusID: "current-old", Kind: agent.RuntimeStatusCurrent, Content: "old current", Seq: 10, BeforeClientMsgID: "trace-1:0"},
		{StatusID: "budget", Kind: agent.RuntimeStatusToolBudget, Content: "round one", Seq: 20, AfterToolCallID: "call-3"},
		{StatusID: "current-new", Kind: agent.RuntimeStatusCurrent, Content: "new current", Seq: 30, BeforeClientMsgID: "trace-2:0"},
	}
	want := []agent.Message{
		{Role: "user", Content: "old current", RuntimeStatus: agent.RuntimeStatusCurrent, RuntimeStatusSeq: 10},
		messages[0], messages[1], messages[2], messages[3], messages[4],
		{Role: "user", Content: "round one", RuntimeStatus: agent.RuntimeStatusToolBudget, RuntimeStatusSeq: 20},
		{Role: "user", Content: "new current", RuntimeStatus: agent.RuntimeStatusCurrent, RuntimeStatusSeq: 30},
		messages[5],
	}

	got, ok := replayRuntimeStatus(messages, lastRow, rows, entries)
	require.True(t, ok)
	assert.Equal(t, want, got)
	requireCompleteToolGroups(t, got)
	after, err := json.Marshal(messages)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
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
	clearErr    error
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
	if f.clearErr != nil {
		return f.clearErr
	}
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

func TestExecutorReplaySessionRuntimeStatus_InvalidGroupCleanupFailure(t *testing.T) {
	messages, lastRow, rows := parallelRuntimeStatusFixture()
	store := &fakeRuntimeStatusStore{
		entries: []model.RuntimeStatus{{
			StatusID: "middle", Kind: agent.RuntimeStatusToolBudget,
			Content: "middle", Seq: 1, AfterToolCallID: "call-2",
		}},
		clearErr: errors.New("redis cleanup failed"),
	}
	executor := &Executor{runtimeStatus: store}

	got := executor.replaySessionRuntimeStatus(t.Context(), "s-1", messages, lastRow, rows)
	assert.Equal(t, messages, got)
	assert.Equal(t, 1, store.clearCalls)
	assert.Len(t, store.entries, 1, "cleanup failure must not make the invalid log usable")
	requireCompleteToolGroups(t, got)
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
