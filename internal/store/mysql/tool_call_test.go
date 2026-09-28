package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

// TestAppendFinalBatch_IdempotentRetryAndOrdering covers refactor-message-
// storage task 1.4: a redelivered round batch collapses to one row set, and
// ListToolCalls returns interactions in (turn, round, call) order.
func TestAppendFinalBatch_IdempotentRetryAndOrdering(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	sessionID := "bbbbbbbb-0000-7000-8000-000000000001"
	turnID := "cccccccc-0000-7000-8000-000000000001"
	now := time.Unix(1_700_000_000, 0).UTC()

	msgs := []model.Message{{
		SessionID: sessionID, MsgTime: now, Agent: model.AgentConfucius,
		MsgIndex: 1, RoundNo: 1, Role: model.RoleAssistant,
		EventType: model.EventTypeMessage, Content: "round one",
		ReasoningContent: "thinking", TraceID: turnID,
		ClientMsgID: turnID + ":1",
	}}
	calls := []model.ToolCall{
		{SessionID: sessionID, TurnID: turnID, RoundNo: 1, ToolCallID: "call_a", Name: "grep", Arguments: "{}", Result: "out a", Status: model.ToolCallStatusCompleted},
		{SessionID: sessionID, TurnID: turnID, RoundNo: 1, ToolCallID: "call_b", Name: "read", Arguments: `{"p":1}`, Result: "out b", Status: model.ToolCallStatusCompleted},
	}

	// Redelivery: the same batch retried after a crash between INSERT and ack.
	require.NoError(t, store.AppendFinalBatch(ctx, msgs, calls))
	require.NoError(t, store.AppendFinalBatch(ctx, msgs, calls))

	gotMsgs, err := store.ListMessages(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, gotMsgs, 1)
	assert.Equal(t, "round one", gotMsgs[0].Content)
	assert.Equal(t, 1, gotMsgs[0].RoundNo)
	assert.Equal(t, "thinking", gotMsgs[0].ReasoningContent)

	gotCalls, err := store.ListToolCalls(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, gotCalls, 2)
	assert.Equal(t, "call_a", gotCalls[0].ToolCallID)
	assert.Equal(t, "out a", gotCalls[0].Result)
	assert.Equal(t, "call_b", gotCalls[1].ToolCallID)
}

// TestAppendFinalBatch_FinalColumnsRoundTrip verifies the final-row columns
// (round_no, status) round-trip through the COALESCEd scan.
func TestAppendFinalBatch_FinalColumnsRoundTrip(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	sessionID := "dddddddd-0000-7000-8000-000000000002"
	turnID := "eeeeeeee-0000-7000-8000-000000000002"
	now := time.Unix(1_700_000_100, 0).UTC()

	msgs := []model.Message{{
		SessionID: sessionID, MsgTime: now, Agent: model.AgentConfucius,
		MsgIndex: 3, RoundNo: 2, Role: model.RoleAssistant,
		EventType: model.EventTypeMessage, Status: model.MessageStatusInterrupted,
		Content: "partial", TraceID: turnID, ClientMsgID: turnID + ":3",
	}}
	require.NoError(t, store.AppendFinalBatch(ctx, msgs, nil))

	got, err := store.ListMessages(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 2, got[0].RoundNo)
	assert.Equal(t, model.MessageStatusInterrupted, got[0].Status)
}
