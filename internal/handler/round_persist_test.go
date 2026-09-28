package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

func testTurnPersistInfo() turnPersistInfo {
	return turnPersistInfo{
		sessionID: "s", userID: "u", traceID: "trace-1",
		userContent: "hi", userMsgTime: time.Unix(1, 0).UTC(),
	}
}

// tooledTurnEvents builds a two-round turn: round 1 narrates, calls one tool,
// gets its result; round 2 is the final answer. Merged: [start, "Hello",
// tool_call, tool_result, "done"] — round 1 opens at ord 1, round 2 at ord 4.
func tooledTurnEvents() []stream.StreamEvent {
	return []stream.StreamEvent{
		stream.AgentStartEvent(stream.AgentConfucius),
		stream.TokenEvent(stream.AgentConfucius, "Hel"),
		stream.TokenEvent(stream.AgentConfucius, "lo"),
		stream.ToolCallEvent(stream.AgentConfucius, "c1", "grep", json.RawMessage(`{"q":"x"}`)),
		stream.ToolResultEvent(stream.AgentConfucius, "c1", "out"),
		stream.TokenEvent(stream.AgentConfucius, "done"),
	}
}

// TestBuildRoundFinalBatch_ClosedRoundsAndDeterministicIDs covers the core
// round-render mechanics (refactor-message-storage): only closed rounds
// persist mid-turn, ids are stable across overlapping renders, tool calls
// pair with results on one row, and the terminal render closes the trailing
// round as a suffix continuing the cursor.
func TestBuildRoundFinalBatch_ClosedRoundsAndDeterministicIDs(t *testing.T) {
	info := testTurnPersistInfo()
	merged := MergeEvents(tooledTurnEvents()) // 5 merged events
	require.Len(t, merged, 5)

	// Mid-turn render (incremental tick): round 1 closes when "done" opens
	// round 2; the trailing round stays in flight.
	batch, consumed := info.buildRoundFinalBatch(merged, 0, false, time.Now().UTC(), roundRenderOptions{})
	require.Equal(t, 4, consumed, "cursor stops at the round-2 opening token")
	require.Len(t, batch.msgs, 2) // user + round-1 final
	require.Len(t, batch.calls, 1)

	round1 := batch.msgs[1]
	assert.Equal(t, "trace-1:1", round1.ClientMsgID)
	assert.Equal(t, 1, round1.RoundNo)
	assert.Equal(t, "Hello", round1.Content)
	assert.Equal(t, model.EventTypeMessage, round1.EventType)
	assert.Equal(t, model.RoleAssistant, round1.Role)
	assert.Equal(t, "grep", batch.calls[0].Name)
	assert.Equal(t, `{"q":"x"}`, batch.calls[0].Arguments)
	assert.Equal(t, "out", batch.calls[0].Result)
	assert.Equal(t, model.ToolCallStatusCompleted, batch.calls[0].Status)
	assert.Equal(t, 1, batch.calls[0].RoundNo)

	// Terminal render from the cursor: round 2 only, id keyed on its opening
	// ordinal; re-rendering the full range yields the same ids (idempotency
	// source for the deterministic UK).
	var flushed turnFlushState
	flushed.mark(consumed)
	suffix, _ := info.buildRoundFinalBatch(merged, flushed.count(), flushed.userPersisted(), time.Now().UTC(),
		roundRenderOptions{terminal: true})
	require.Len(t, suffix.msgs, 1)
	assert.Equal(t, "trace-1:4", suffix.msgs[0].ClientMsgID)
	assert.Equal(t, 2, suffix.msgs[0].RoundNo)
	assert.Equal(t, "done", suffix.msgs[0].Content)

	again, _ := info.buildRoundFinalBatch(merged, 0, false, time.Now().UTC(), roundRenderOptions{terminal: true})
	assert.Equal(t, "trace-1:4", again.msgs[len(again.msgs)-1].ClientMsgID)
}

// TestBuildRoundFinalBatch_UserPersistedGating covers the send-time user-row
// gating in three states: fresh turn (user row included), send-time write
// landed (pure round suffix from 0), cursor past 0 (suffix only regardless of
// the flag).
func TestBuildRoundFinalBatch_UserPersistedGating(t *testing.T) {
	info := testTurnPersistInfo()
	events := []stream.StreamEvent{
		stream.AgentStartEvent(stream.AgentConfucius),
		stream.TokenEvent(stream.AgentConfucius, "one"),
		stream.TokenEvent(stream.AgentConfucius, "two"),
	}
	merged := MergeEvents(events)

	fresh, _ := info.buildRoundFinalBatch(merged, 0, false, time.Now().UTC(), roundRenderOptions{terminal: true})
	require.Len(t, fresh.msgs, 2)
	assert.Equal(t, model.RoleUser, fresh.msgs[0].Role)
	assert.Equal(t, "trace-1:0", fresh.msgs[0].ClientMsgID)

	sendTimeLanded, _ := info.buildRoundFinalBatch(merged, 0, true, time.Now().UTC(), roundRenderOptions{terminal: true})
	require.Len(t, sendTimeLanded.msgs, 1)
	assert.NotEqual(t, model.RoleUser, sendTimeLanded.msgs[0].Role)

	// from=1 is a round-boundary cursor: the round OPENING at ord 1 is the
	// next un-persisted round and renders.
	from1, _ := info.buildRoundFinalBatch(merged, 1, false, time.Now().UTC(), roundRenderOptions{terminal: true})
	require.Len(t, from1.msgs, 1)
}

// TestBuildRoundFinalBatch_InterruptedTerminal marks the final round truncated
// on the cancel/error path (interrupted-turn-persistence).
func TestBuildRoundFinalBatch_InterruptedTerminal(t *testing.T) {
	info := testTurnPersistInfo()
	merged := MergeEvents(tooledTurnEvents())

	batch, _ := info.buildRoundFinalBatch(merged, 0, false, time.Now().UTC(),
		roundRenderOptions{terminal: true, interrupted: true})
	require.Len(t, batch.msgs, 3) // user + round1 + round2(truncated)
	last := batch.msgs[2]
	assert.Equal(t, model.MessageStatusInterrupted, last.Status)
	assert.Empty(t, batch.msgs[1].Status, "only the truncated final round carries the mark")
}

// TestBuildRoundFinalBatch_SubAgentEventsExcluded pins the sub-agent split:
// events carrying agent_instance_id never produce messages/tool_calls rows.
func TestBuildRoundFinalBatch_SubAgentEventsExcluded(t *testing.T) {
	info := testTurnPersistInfo()
	sub := stream.TokenEvent(stream.AgentConfucius, "sub says")
	sub.Meta = map[string]any{stream.MetaAgentInstanceID: "inst-1", stream.MetaParentToolCallID: "call_x"}
	events := append(tooledTurnEvents(), sub)
	merged := MergeEvents(events)

	batch, _ := info.buildRoundFinalBatch(merged, 0, false, time.Now().UTC(), roundRenderOptions{terminal: true})
	for _, m := range batch.msgs {
		assert.Empty(t, m.AgentInstanceID)
		assert.NotEqual(t, "sub says", m.Content)
	}
}

// TestRoundPersistSequence_UserRowExactlyOnce simulates the full batch
// sequence of a tooled turn under send-time persistence: send-time [user] →
// round flush [round 1] → turn-end [round 2]. Concatenated, the batches
// reproduce the turn exactly once.
func TestRoundPersistSequence_UserRowExactlyOnce(t *testing.T) {
	info := testTurnPersistInfo()
	merged := MergeEvents(tooledTurnEvents())
	var flushed turnFlushState

	sendBatch := []model.Message{UserMessage(info.sessionID, info.traceID, info.userContent, info.userMsgTime)}
	flushed.markUserPersisted()

	midBatch, consumed := info.buildRoundFinalBatch(merged, flushed.count(), flushed.userPersisted(),
		time.Now().UTC(), roundRenderOptions{})
	require.Equal(t, 4, consumed)
	flushed.mark(consumed)

	endBatch, _ := info.buildRoundFinalBatch(merged, flushed.count(), flushed.userPersisted(),
		time.Now().UTC(), roundRenderOptions{terminal: true})

	all := append(append(sendBatch, midBatch.msgs...), endBatch.msgs...)
	require.Len(t, all, 3, "user + two round finals, nothing duplicated or dropped")
	seen := map[string]int{}
	for _, m := range all {
		seen[m.ClientMsgID]++
	}
	for id, n := range seen {
		assert.Equal(t, 1, n, "client_msg_id %s appears exactly once", id)
	}
	totalCalls := len(midBatch.calls) + len(endBatch.calls)
	assert.Equal(t, 1, totalCalls)
}
