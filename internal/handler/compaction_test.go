package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/stream"
)

// compactionRows builds an alternating user/token persistence history: 2*n
// rows that reconstruct to exactly 2*n agent messages (row i ↔ agent message
// i). Row IDs and times ascend so composite cursors are unambiguous.
func compactionRows(n int) []model.Message {
	base := time.Unix(1_700_000_000, 0).UTC()
	rows := make([]model.Message, 0, 2*n)
	for i := 0; i < n; i++ {
		rows = append(rows, model.Message{
			ID:        int64(2*i + 1),
			MsgTime:   base.Add(time.Duration(2*i) * time.Minute),
			MsgIndex:  0,
			SessionID: "sess-1",
			Agent:     model.AgentUser,
			Role:      model.RoleUser,
			EventType: model.EventTypeMessage,
			Content:   "user turn " + strings.Repeat("x", i+1),
			TraceID:   "trace-old",
		})
		rows = append(rows, model.Message{
			ID:        int64(2*i + 2),
			MsgTime:   base.Add(time.Duration(2*i+1) * time.Minute),
			MsgIndex:  1,
			SessionID: "sess-1",
			Agent:     model.AgentConfucius,
			Role:      model.RoleAssistant,
			EventType: model.EventTypeToken,
			Content:   "assistant reply " + strings.Repeat("y", i+1),
			TraceID:   "trace-old",
		})
	}
	return rows
}

// newCompactionHandlerEnv mirrors newSessionHandlerEnv but wires a real
// CompactionService (backed by the same fakes plus the given LLM) so the
// streaming handler's stitching and turn-start paths run in-process.
func newCompactionHandlerEnv(t *testing.T, stub *stubOrchestrator, llm agent.LLMClient, maxContextTokens int) (*sessionHandlerTestEnv, *service.CompactionService) {
	t.Helper()
	if stub == nil {
		stub = &stubOrchestrator{eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfucius),
			stream.TokenEvent(stream.AgentConfucius, "ok"),
			stream.AgentEndEvent(stream.AgentConfucius),
		}}
	}
	mysql := &handlerFakeMySQL{getSessionByIDFound: &model.Session{SessionID: "sess-1", UserID: "user-1"}}
	redis := &handlerFakeRedis{}
	fs := newHandlerFakeFS()
	deps := sessionDeps(mysql, redis, fs)
	sessSvc := newSessionSvc(deps)
	msgSvc := newMessageSvc(deps)
	titleSvc := service.NewTitleService(nil, mysql, config.OpenAIConfig{TitleModel: "title-model"})

	compSvc := service.NewCompactionService(deps, llm, "gpt-test", maxContextTokens)

	h := NewSessionHandler(sessSvc, titleSvc, nil)
	streamH := NewMessageStreamHandler(sessSvc, msgSvc, titleSvc, compSvc, stub, "/tmp/blowball-test-data", run.NewManager(run.NewMemStore(), run.NewRegistry()), testSelectionConfigWindow(maxContextTokens), 0)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-1")
		c.Next()
	})
	r.POST("/api/v1/sessions/:session_id/messages", streamH.SendMessage)
	return &sessionHandlerTestEnv{h: h, stream: streamH, mysql: mysql, redis: redis, fs: fs, stub: stub, engine: r}, compSvc
}

// postToSession posts one message and returns the recorder.
func postToSession(t *testing.T, env *sessionHandlerTestEnv, content string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-1/messages",
		strings.NewReader(`{"content":"`+content+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	return w
}

// cannedSummaryLLM returns one fixed summary response (the compaction call).
type cannedSummaryLLM struct{ content string }

func (c *cannedSummaryLLM) StreamChat(context.Context, agent.LLMRequest, func(string) error, func(string) error) (agent.LLMResponse, error) {
	return agent.LLMResponse{Content: c.content, Usage: agent.Usage{PromptTokens: 50, CompletionTokens: 5}}, nil
}

// TestStitchCompacted_Unit covers the pure stitching helper: first message
// verbatim, framed summary as a user message, tail from the post-boundary
// rows; and the nil degradations.
func TestStitchCompacted_Unit(t *testing.T) {
	rows := compactionRows(8) // 16 rows, 16 agent messages
	agentMsgs, lastRow, err := MessagesToAgentMessagesIndexed(rows)
	require.NoError(t, err)
	require.Len(t, agentMsgs, 16)
	require.Len(t, lastRow, 16)

	// Boundary after agent message 6 → tail = messages 7..15.
	boundaryRow := rows[lastRow[6]]
	rec := &model.ContextCompaction{
		Content:          "CHECKPOINT BODY",
		BoundaryMsgTime:  boundaryRow.MsgTime,
		BoundaryMsgIndex: boundaryRow.MsgIndex,
		BoundaryMsgID:    boundaryRow.ID,
	}

	stitched := stitchCompacted(rows, agentMsgs, rec)
	require.NotNil(t, stitched)
	require.Len(t, stitched, 2+9) // first + summary + 9 tail messages
	require.Equal(t, agentMsgs[0], stitched[0], "first message must survive verbatim")
	require.Equal(t, "user", stitched[1].Role)
	require.Contains(t, stitched[1].Content, "<compacted-summary>")
	require.Contains(t, stitched[1].Content, "CHECKPOINT BODY")
	require.Contains(t, stitched[1].Content, summaryLeadIn)
	require.Equal(t, agentMsgs[7], stitched[2], "tail starts right after the boundary")
	require.Equal(t, agentMsgs[15], stitched[len(stitched)-1])
	shadowed := map[string]bool{}
	for _, m := range agentMsgs[1:7] {
		shadowed[m.Content] = true
	}
	for _, m := range stitched {
		require.False(t, shadowed[m.Content], "shadowed middle content %q must not appear", m.Content)
	}

	// Degradations: unknown boundary row and nil record → nil (full history).
	ghost := &model.ContextCompaction{BoundaryMsgID: 99999, BoundaryMsgTime: time.Now(), BoundaryMsgIndex: 0}
	require.Nil(t, stitchCompacted(rows, agentMsgs, ghost))
	require.Nil(t, stitchCompacted(rows, agentMsgs, nil))
}

// TestSendMessage_CompactedFlagStitchesFromCache drives the streaming handler
// with sessions.context_compacted=1 and a cached record: the orchestrator
// must receive first + framed summary + tail + the new user message — nothing
// from the shadowed middle.
func TestSendMessage_CompactedFlagStitchesFromCache(t *testing.T) {
	env, compSvc := newCompactionHandlerEnv(t, nil, &cannedSummaryLLM{content: "x"}, 1000)
	require.True(t, compSvc.Enabled())

	rows := compactionRows(8)
	env.mysql.listMessagesRows = rows
	env.mysql.getSessionByIDFound.ContextCompacted = true

	// Boundary after the 6th agent message; cache the record in Redis.
	agentMsgs, lastRow, err := MessagesToAgentMessagesIndexed(rows)
	require.NoError(t, err)
	boundaryRow := rows[lastRow[6]]
	raw, err := json.Marshal(model.ContextCompaction{
		Content:          "CACHED CHECKPOINT",
		BoundaryMsgTime:  boundaryRow.MsgTime,
		BoundaryMsgIndex: boundaryRow.MsgIndex,
		BoundaryMsgID:    boundaryRow.ID,
	})
	require.NoError(t, err)
	env.redis.compactionCache = raw

	w := postToSession(t, env, "next question")
	require.Equal(t, http.StatusOK, w.Code)

	got := env.stub.gotMessages
	require.GreaterOrEqual(t, len(got), 4, "stitched context + new user message")
	require.Equal(t, agentMsgs[0], got[0])
	require.Equal(t, "user", got[1].Role)
	require.Contains(t, got[1].Content, "CACHED CHECKPOINT")
	require.Equal(t, agentMsgs[7], got[2], "tail begins after the boundary")
	require.Equal(t, "next question", got[len(got)-1].Content)
	shadowed := map[string]bool{}
	for _, m := range agentMsgs[1:7] {
		shadowed[m.Content] = true
	}
	for _, m := range got {
		require.False(t, shadowed[m.Content], "shadowed middle content %q must not reach the orchestrator", m.Content)
	}
	// Cache hit: no MySQL compaction read, no new compaction record.
	require.Equal(t, 1, env.redis.getCompactionCalls)
	require.Equal(t, 0, len(env.mysql.compactions))
}

// TestSendMessage_UncompactedFlagSendsFullHistory pins the flag=0 behavior:
// the full reconstruction reaches the orchestrator.
func TestSendMessage_UncompactedFlagSendsFullHistory(t *testing.T) {
	env, _ := newCompactionHandlerEnv(t, nil, &cannedSummaryLLM{content: "x"}, 1000)
	rows := compactionRows(4)
	env.mysql.listMessagesRows = rows

	w := postToSession(t, env, "next question")
	require.Equal(t, http.StatusOK, w.Code)

	got := env.stub.gotMessages
	require.Len(t, got, 9) // 8 history + current user
	require.Equal(t, "user turn xxxx", got[6].Content)
	require.Equal(t, "next question", got[8].Content)
}

// TestSendMessage_MissingRecordDegradesToFullHistory: flag=1 but both cache
// and table miss → full history, turn proceeds (spec: "Missing record
// degrades to full history").
func TestSendMessage_MissingRecordDegradesToFullHistory(t *testing.T) {
	env, _ := newCompactionHandlerEnv(t, nil, &cannedSummaryLLM{content: "x"}, 1000)
	env.mysql.listMessagesRows = compactionRows(4)
	env.mysql.getSessionByIDFound.ContextCompacted = true

	w := postToSession(t, env, "next question")
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, env.stub.gotMessages, 9, "full unreconstructed-compaction history")
}

// TestSendMessage_CompactionDisabledIgnoresFlag: the rollback path — the flag
// is set but max_context_tokens is 0, so no stitching at all.
func TestSendMessage_CompactionDisabledIgnoresFlag(t *testing.T) {
	env, compSvc := newCompactionHandlerEnv(t, nil, &cannedSummaryLLM{content: "x"}, 0)
	require.False(t, compSvc.Enabled())
	env.mysql.listMessagesRows = compactionRows(4)
	env.mysql.getSessionByIDFound.ContextCompacted = true

	w := postToSession(t, env, "next question")
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, env.stub.gotMessages, 9, "disabled compaction sends full history")
}

// TestSendMessage_TurnStartPreventiveCompaction: the previous turn's recorded
// context_tokens exceeds the threshold, so the handler compacts BEFORE the
// orchestrator runs and stitches the same turn's context from the fresh
// record.
func TestSendMessage_TurnStartPreventiveCompaction(t *testing.T) {
	env, _ := newCompactionHandlerEnv(t, nil, &cannedSummaryLLM{content: "FRESH CHECKPOINT"}, 1000)
	rows := compactionRows(8)
	env.mysql.listMessagesRows = rows
	env.mysql.latestContextTokens = 900 // ≥ 80% of 1000

	w := postToSession(t, env, "next question")
	require.Equal(t, http.StatusOK, w.Code)

	require.Len(t, env.mysql.compactions, 1, "turn-start compaction must write one record")
	rec := env.mysql.compactions[0]
	require.Equal(t, model.CompactionTriggerTurnStart, rec.TriggerKind)
	require.Equal(t, 900, rec.TriggerTokens)
	require.Equal(t, 1, env.mysql.updateSessionCompactedCalls, "flag is set last")

	got := env.stub.gotMessages
	require.GreaterOrEqual(t, len(got), 4)
	require.Contains(t, got[1].Content, "FRESH CHECKPOINT")
	require.Equal(t, "next question", got[len(got)-1].Content)
	// The middle was re-summarized from scratch: no prior middle content is
	// fed to the orchestrator except inside the checkpoint frame.
	for _, m := range got[1 : len(got)-1] {
		require.NotEqual(t, "user turn xxxx", m.Content, "shadowed middle must not reach the orchestrator")
	}
}

// TestTurnPersistMessages_DeterministicIDsAndSuffixSplit covers the mid-turn
// flush idempotency mechanics: deterministic client_msg_ids stable across
// overlapping renders, and the suffix split after a flush.
func TestTurnPersistMessages_DeterministicIDsAndSuffixSplit(t *testing.T) {
	events := []stream.StreamEvent{
		stream.AgentStartEvent(stream.AgentConfucius),
		stream.TokenEvent(stream.AgentConfucius, "Hel"),
		stream.TokenEvent(stream.AgentConfucius, "lo"),
		stream.ToolCallEvent(stream.AgentConfucius, "c1", "t", json.RawMessage(`{}`)),
		stream.ToolResultEvent(stream.AgentConfucius, "c1", "out"),
		stream.TokenEvent(stream.AgentConfucius, "done"),
	}
	merged := MergeEvents(events) // start, "Hello", tool_call, tool_result, "done" = 5

	info := turnPersistInfo{
		sessionID: "s", userID: "u", traceID: "trace-1",
		userContent: "hi", userMsgTime: time.Unix(1, 0).UTC(),
	}

	full, err := info.buildTurnMessages(merged, 0, time.Now().UTC())
	require.NoError(t, err)
	require.Len(t, full, 6) // user + 5 events
	require.Equal(t, "trace-1:0", full[0].ClientMsgID)
	for i, want := range []string{"trace-1:1", "trace-1:2", "trace-1:3", "trace-1:4", "trace-1:5"} {
		require.Equal(t, want, full[i+1].ClientMsgID, "merged event %d", i)
	}

	// A flush persisted the first 3 merged events; the turn-end save must
	// render exactly the suffix with CONTINUING ids and no user row.
	var flushed turnFlushState
	flushed.mark(3)
	suffix, err := info.buildTurnMessages(merged, flushed.count(), time.Now().UTC())
	require.NoError(t, err)
	require.Len(t, suffix, 2)
	require.Equal(t, "trace-1:4", suffix[0].ClientMsgID)
	require.Equal(t, "trace-1:5", suffix[1].ClientMsgID)

	// Re-rendering the full range yields the SAME ids (idempotency source).
	again, err := info.buildTurnMessages(merged, 0, time.Now().UTC())
	require.NoError(t, err)
	for i := range full {
		require.Equal(t, full[i].ClientMsgID, again[i].ClientMsgID)
	}
}

// TestTurnEventTap_SnapshotAndClose covers the tap contract: a served
// snapshot returns the collected events; after close (turn over) Snapshot
// returns nil so callers abort the flush.
func TestTurnEventTap_SnapshotAndClose(t *testing.T) {
	tap := NewTurnEventTap()
	ctx := context.Background()

	go func() {
		req := <-tap.requests()
		tap.serve(req, []stream.StreamEvent{stream.TokenEvent(stream.AgentConfucius, "x")})
	}()
	got := tap.Snapshot(ctx)
	require.Len(t, got, 1)

	tap.close()
	require.Nil(t, tap.Snapshot(ctx), "closed tap must yield nil snapshots")
}

// TestBuildTurnUsage_ContextTokens checks meta.context_tokens flows into the
// turn_usage row, and (per-request-model-selection) that the turn's resolved
// model name rides the row.
func TestBuildTurnUsage_ContextTokens(t *testing.T) {
	usage := map[string]any{
		"total": map[string]any{"total_tokens": float64(42)},
		"meta":  map[string]any{"context_tokens": float64(37), "parallel": false},
	}
	tu, ok := buildTurnUsage("s", "t", "u", "glm-4.7", usage)
	require.True(t, ok)
	require.Equal(t, 42, tu.TotalTokens)
	require.Equal(t, 37, tu.ContextTokens)
	require.Equal(t, "glm-4.7", tu.Model, "the turn's resolved model must ride the row")

	// Absent meta/context_tokens → 0, not a failure. An empty model is kept
	// verbatim (the store's NULLIF maps it to NULL).
	tu2, ok := buildTurnUsage("s", "t", "u", "", map[string]any{
		"total": map[string]any{"total_tokens": 3},
	})
	require.True(t, ok)
	require.Equal(t, 0, tu2.ContextTokens)
	require.Empty(t, tu2.Model)
}
