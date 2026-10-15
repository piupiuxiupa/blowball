package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/memory"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

const memoryLeadInForTest = "Long-term memories about this user, recalled from earlier sessions by relevance to the new message. Treat them as background context; rely on them without re-asking, but do not recite them."

func recallResult(entries ...memory.RecallEntry) memory.RecallResult {
	frame := "<user-memories>\n" + memoryLeadInForTest
	for _, e := range entries {
		frame += "\n" + e.Line
	}
	return memory.RecallResult{Entries: entries, Frame: frame + "\n</user-memories>"}
}

// TestRecallInjection_DeterministicIdentity: the pair's tool_call_id and
// arguments are pure functions of (session, turn, result) — redelivery of
// the send-time batch mints identical rows the unique keys collapse, while
// a different turn mints a distinct id.
func TestRecallInjection_DeterministicIdentity(t *testing.T) {
	res := recallResult(memory.RecallEntry{Key: "viking://u/mem/a", Line: "- [memory 87%] fact a"})
	a := newRecallInjection("sess-1", "trace-1", "q", res)
	b := newRecallInjection("sess-1", "trace-1", "q", res)
	c := newRecallInjection("sess-1", "trace-2", "q", res)

	assert.Equal(t, a.callID, b.callID)
	assert.Equal(t, a.args, b.args)
	assert.NotEqual(t, a.callID, c.callID)
	assert.Contains(t, a.args, `"entries":["viking://u/mem/a"]`)

	ph, call := a.persistRows("sess-1", "trace-1", time.Unix(0, 0).UTC())
	assert.Equal(t, "trace-1:-1", ph.ClientMsgID)
	assert.Equal(t, a.callID, ph.ToolCallID)
	assert.Equal(t, a.callID, call.ToolCallID)
	assert.Equal(t, a.args, call.Arguments)
	assert.Equal(t, a.frame, call.Result)
	assert.Equal(t, model.ToolCallStatusCompleted, call.Status)
	assert.Equal(t, "trace-1", call.TurnID)
}

// TestInjectedRecallKeys_Derivation: the session's injected set derives from
// the recovered transcript — every historical recall_memories call's
// arguments contribute its entries; malformed arguments and unrelated tools
// are ignored.
func TestInjectedRecallKeys_Derivation(t *testing.T) {
	res := recallResult(
		memory.RecallEntry{Key: "viking://u/mem/a", Line: "- [memory 87%] a"},
		memory.RecallEntry{Key: "sha256:abc", Line: "- [memory 80%] b"},
	)
	history := append(newRecallInjection("s", "t1", "q", res).agentMessages(),
		agent.Message{Role: "user", Content: "q"},
		agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID:       "other",
			Function: agent.ToolCallFunction{Name: "executor_bash", Arguments: `{"cmd":"ls"}`},
		}}},
		agent.Message{Role: "tool", ToolCallID: "other", Name: "executor_bash", Content: "ok"},
		agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID:       "bad",
			Function: agent.ToolCallFunction{Name: recallToolName, Arguments: `{not json`},
		}}},
	)

	assert.Equal(t, map[string]struct{}{
		"viking://u/mem/a": {},
		"sha256:abc":       {},
	}, injectedRecallKeys(history))
	assert.Nil(t, injectedRecallKeys(nil))
}

// TestRecallPairAppendOnlyReplay is the append-only core contract: the rows
// persisted for turn 1 reconstruct byte-identically to what the orchestrator
// received, and turn 2's payload extends — never rewrites — that prefix.
func TestRecallPairAppendOnlyReplay(t *testing.T) {
	sessionID := "sess-1"
	trace1 := "01920000-0000-7000-8000-000000000001"
	trace2 := "01920000-0000-7000-8000-000000000002"
	ts := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	// Turn 1: pair + user sent to the orchestrator...
	res1 := recallResult(memory.RecallEntry{Key: "viking://u/mem/a", Line: "- [memory 87%] fact a"})
	inj1 := newRecallInjection(sessionID, trace1, "question one", res1)
	sent1 := append(inj1.agentMessages(), agent.Message{Role: "user", Content: "question one"})

	// ...then persisted exactly as the send-time / turn-end paths write rows.
	ph1, call1 := inj1.persistRows(sessionID, trace1, ts)
	rows1 := []model.Message{
		ph1,
		UserMessage(sessionID, trace1, "question one", ts),
		{
			SessionID: sessionID, MsgTime: ts.Add(time.Second), Agent: model.AgentConfucius,
			MsgIndex: 1, Role: model.RoleAssistant, EventType: model.EventTypeMessage,
			Content: "answer one", TraceID: trace1, ClientMsgID: trace1 + ":1",
		},
	}

	// Restart / next-turn recovery: reconstruct from the durable rows alone.
	got1, _, err := MessagesToAgentMessagesIndexed(rows1, []model.ToolCall{call1})
	require.NoError(t, err)
	want1 := append(append([]agent.Message{}, sent1...), agent.Message{Role: "assistant", Content: "answer one"})
	assert.Equal(t, want1, got1, "persisted rows must replay the sent pair byte-identically")

	// Turn 2: the derived set suppresses the already-injected entry; only a
	// NEW entry renders, and the turn-2 payload's prefix is exactly got1.
	require.Equal(t, map[string]struct{}{"viking://u/mem/a": {}}, injectedRecallKeys(got1))
	res2 := recallResult(
		memory.RecallEntry{Key: "viking://u/mem/a", Line: "- [memory 90%] fact a"},
		memory.RecallEntry{Key: "viking://u/mem/b", Line: "- [memory 75%] fact b"},
	)
	var newEntries []memory.RecallEntry
	for _, e := range res2.Entries {
		if _, ok := injectedRecallKeys(got1)[e.Key]; !ok {
			newEntries = append(newEntries, e)
		}
	}
	require.Len(t, newEntries, 1, "already-injected entry must be filtered")
	inj2 := newRecallInjection(sessionID, trace2, "question two", recallResult(newEntries...))
	sent2 := append(append([]agent.Message{}, got1...), inj2.agentMessages()...)
	sent2 = append(sent2, agent.Message{Role: "user", Content: "question two"})
	assert.Equal(t, got1, sent2[:len(got1)], "turn 2 extends the prefix; it never rewrites it")
}

// TestMemoryRecall_SecondTurnNoNewEntriesNoInjection: with durable turn-1
// rows recovered (Redis expired, MySQL tier) and the memory store returning
// the SAME entry, the turn injects nothing new — the payload is replayed
// history plus the user message, and the replayed pair is the only
// recall_memories unit in context.
func TestMemoryRecall_SecondTurnNoNewEntriesNoInjection(t *testing.T) {
	ov := newOVCapture() // always returns viking://user/user-1/memories/prefs
	srv := httptest.NewServer(ov)
	defer srv.Close()

	stub := &stubOrchestrator{eventsToEmit: []stream.StreamEvent{
		stream.TokenEvent(stream.AgentConfucius, "ok"),
	}}
	env := newMemoryHandlerEnv(t, stub, srv.URL)

	// Durable turn-1 rows (Redis expired; the MySQL tier serves recovery).
	trace0 := "01920000-0000-7000-8000-000000000000"
	ts := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	res := recallResult(memory.RecallEntry{
		Key:  "viking://user/user-1/memories/prefs",
		Line: "- [memory 87%] User runs fish shell on macOS.",
	})
	inj := newRecallInjection("sess-1", trace0, "first question", res)
	ph, call := inj.persistRows("sess-1", trace0, ts)
	env.mysql.listMessagesRows = []model.Message{
		ph,
		UserMessage("sess-1", trace0, "first question", ts),
		{
			SessionID: "sess-1", MsgTime: ts.Add(time.Second), Agent: model.AgentConfucius,
			MsgIndex: 1, Role: model.RoleAssistant, EventType: model.EventTypeMessage,
			Content: "first answer", TraceID: trace0, ClientMsgID: trace0 + ":1",
		},
	}
	env.mysql.listToolCallsRows = []model.ToolCall{call}

	w := postToSession(t, env, "another question")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	msgs := stub.messages()
	require.NotEmpty(t, msgs)
	assert.Equal(t, "user", msgs[len(msgs)-1].Role)
	assert.Contains(t, msgs[len(msgs)-1].Content, "another question")

	// Exactly one recall unit (the replayed turn-1 pair); no NEW injection.
	recallTools := 0
	for _, m := range msgs {
		if m.Role == "tool" && m.Name == recallToolName {
			recallTools++
		}
	}
	assert.Equal(t, 1, recallTools, "same entry must not be re-injected; only the replayed pair remains")

	// And the send-time batch wrote no new recall pair for this turn.
	batches, calls := env.mysql.finalBatchSnapshots()
	for _, b := range batches {
		for _, m := range b {
			assert.NotEqual(t, "trace-1:-1", m.ClientMsgID, "no pair must persist when nothing is new")
		}
	}
	for _, cs := range calls {
		for _, c := range cs {
			assert.NotEqual(t, "trace-1", c.TurnID, "no recall call row for a no-new-entries turn")
		}
	}

	// Sanity: the args round-trip through the persisted pair.
	var args recallArgs
	require.NoError(t, json.Unmarshal([]byte(inj.args), &args))
	assert.Equal(t, []string{"viking://user/user-1/memories/prefs"}, args.Entries)
}
