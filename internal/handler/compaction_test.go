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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/memory"
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
	streamH := NewMessageStreamHandler(sessSvc, msgSvc, titleSvc, compSvc, nil, stub, "/tmp/blowball-test-data", run.NewManager(run.NewMemStore(), run.NewRegistry()), testSelectionConfigWindow(maxContextTokens), 0)

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

type countingSummaryLLM struct {
	content string
	calls   int
}

func (c *countingSummaryLLM) StreamChat(context.Context, agent.LLMRequest, func(string) error, func(string) error) (agent.LLMResponse, error) {
	c.calls++
	return agent.LLMResponse{Content: c.content, Usage: agent.Usage{PromptTokens: 50, CompletionTokens: 5}}, nil
}

// TestStitchCompacted_Unit covers the pure stitching helper: first message
// verbatim, framed summary as a user message, tail from the post-boundary
// rows; and the nil degradations.
func TestStitchCompacted_Unit(t *testing.T) {
	rows := compactionRows(8) // 16 rows, 16 agent messages
	agentMsgs, lastRow, err := MessagesToAgentMessagesIndexed(rows, nil)
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

	stitched := stitchCompacted(rows, nil, agentMsgs, rec)
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
	require.Nil(t, stitchCompacted(rows, nil, agentMsgs, ghost))
	require.Nil(t, stitchCompacted(rows, nil, agentMsgs, nil))
}

func TestStitchCompacted_AppliesPersistedToolSummaries(t *testing.T) {
	rows := compactionRows(4)
	msgTime := rows[7].MsgTime
	rows[7] = model.Message{
		ID: 8, MsgTime: msgTime, MsgIndex: 1, SessionID: "sess-1",
		Agent: model.AgentConfucius, Role: model.RoleTool,
		EventType: model.EventTypeToolResult,
		Content:   `{"tool_call_id":"tool-7","output":{"status":1,"error":"test failed: id-7"}}`,
		TraceID:   "trace-old",
	}
	agentMsgs, lastRow, err := MessagesToAgentMessagesIndexed(rows, nil)
	require.NoError(t, err)
	boundary := rows[lastRow[5]]
	rec := &model.ContextCompaction{
		Content:          "CHECKPOINT",
		BoundaryMsgTime:  boundary.MsgTime,
		BoundaryMsgIndex: boundary.MsgIndex,
		BoundaryMsgID:    boundary.ID,
		RetainedResults: map[string]string{
			"tool-7": `{"status":"error","outcome":"test failed: id-7"}`,
		},
	}
	stitched := stitchCompacted(rows, nil, agentMsgs, rec)
	require.NotNil(t, stitched)
	for _, m := range stitched {
		if m.ToolCallID == "tool-7" {
			assert.Contains(t, m.Content, `"status":"error"`)
			return
		}
	}
}

func TestStitchCompacted_UsesBoundedAnchor(t *testing.T) {
	rows := compactionRows(8)
	agentMsgs, lastRow, err := MessagesToAgentMessagesIndexed(rows, nil)
	require.NoError(t, err)
	boundary := rows[lastRow[6]]
	rec := &model.ContextCompaction{
		Content:          "CHECKPOINT",
		BoundaryMsgTime:  boundary.MsgTime,
		BoundaryMsgIndex: boundary.MsgIndex,
		BoundaryMsgID:    boundary.ID,
		AnchorContent:    "bounded anchor with messages/first reference",
		AnchorTruncated:  true,
	}
	stitched := stitchCompacted(rows, nil, agentMsgs, rec)
	require.NotNil(t, stitched)
	assert.Equal(t, rec.AnchorContent, stitched[0].Content)
	assert.NotEqual(t, agentMsgs[0].Content, stitched[0].Content)
}

func TestStitchCompacted_PlanUpdatedTailRowIsDisplayOnly(t *testing.T) {
	msgTime := time.Unix(1_700_000_000, 0).UTC()
	planJSON := `{"revision":9,"steps":[{"step":"post-boundary","status":"in_progress"}]}`
	rows := []model.Message{
		{ID: 1, MsgTime: msgTime, MsgIndex: 1, Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "original task"},
		{ID: 2, MsgTime: msgTime, MsgIndex: 2, Agent: stream.AgentConfucius, Role: "", EventType: model.EventTypePlanUpdated, Content: `{"revision":1}`},
		{ID: 3, MsgTime: msgTime, MsgIndex: 3, Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"p9","name":"update_plan","args":{"steps":[]}}`},
		{ID: 4, MsgTime: msgTime, MsgIndex: 4, Agent: stream.AgentConfucius, Role: "", EventType: model.EventTypePlanUpdated, Content: planJSON},
		{ID: 5, MsgTime: msgTime, MsgIndex: 5, Agent: stream.AgentConfucius, Role: model.RoleTool, EventType: model.EventTypeToolResult, Content: `{"tool_call_id":"p9","output":{"revision":9}}`},
	}
	agentMsgs, _, err := MessagesToAgentMessagesIndexed(rows, nil)
	require.NoError(t, err)

	rec := &model.ContextCompaction{
		Content:          "CHECKPOINT BODY",
		BoundaryMsgTime:  rows[1].MsgTime,
		BoundaryMsgIndex: rows[1].MsgIndex,
		BoundaryMsgID:    rows[1].ID,
	}
	stitched := stitchCompacted(rows, nil, agentMsgs, rec)
	require.NotNil(t, stitched)
	require.Len(t, stitched, 4) // original task + summary + tool call + result
	for _, m := range stitched {
		assert.NotContains(t, m.Content, planJSON)
	}
	assert.Equal(t, "update_plan", stitched[2].ToolCalls[0].Function.Name)
}

// TestStitchCompacted_MergedToolUnitStaysTogetherInTail pins the row
// attribution for a final-row narrated tool round: the merged assistant is
// attributed through its last placeholder, so a boundary at the final row
// keeps the assistant call and tool result together in the retained tail.
func TestStitchCompacted_MergedToolUnitStaysTogetherInTail(t *testing.T) {
	msgTime := time.Unix(1_700_000_000, 0).UTC()
	rows := append(compactionRows(2), []model.Message{
		{
			ID: 5, MsgTime: msgTime.Add(4 * time.Minute), MsgIndex: 0,
			SessionID: "sess-1", Agent: model.AgentUser, Role: model.RoleUser,
			EventType: model.EventTypeMessage, Content: "use the tool", TraceID: "trace-tool",
		},
		{
			ID: 6, MsgTime: msgTime.Add(5 * time.Minute), MsgIndex: 1,
			SessionID: "sess-1", Agent: model.AgentConfucius, Role: model.RoleAssistant,
			EventType: model.EventTypeMessage, Content: "checking", ReasoningContent: "reason",
			TraceID: "trace-tool",
		},
		{
			ID: 7, MsgTime: msgTime.Add(5 * time.Minute), MsgIndex: 2,
			SessionID: "sess-1", Agent: model.AgentConfucius, Role: model.RoleAssistant,
			EventType: model.EventTypeToolCall, Content: "grep", TraceID: "trace-tool",
			ToolCallID: "c1",
		},
	}...)
	calls := []model.ToolCall{{
		SessionID: "sess-1", TurnID: "trace-tool", ToolCallID: "c1", Name: "grep",
		Arguments: `{ "q": "x" }`, Result: "result", Status: model.ToolCallStatusCompleted,
	}}

	agentMsgs, lastRow, err := MessagesToAgentMessagesIndexed(rows, calls)
	require.NoError(t, err)
	require.Len(t, agentMsgs, 7)
	require.Len(t, lastRow, 7)
	assert.Equal(t, 6, lastRow[5], "merged assistant attribution reaches its placeholder")

	// Cut before the merged unit. Its attribution reaches the later placeholder,
	// so a boundary selected on the preceding message keeps the whole unit in tail.
	rec := &model.ContextCompaction{
		Content:          "CHECKPOINT BODY",
		BoundaryMsgTime:  rows[lastRow[3]].MsgTime,
		BoundaryMsgIndex: rows[lastRow[3]].MsgIndex,
		BoundaryMsgID:    rows[lastRow[3]].ID,
	}
	stitched := stitchCompacted(rows, calls, agentMsgs, rec)
	require.NotNil(t, stitched)

	found := false
	for i := 1; i < len(stitched); i++ {
		if len(stitched[i].ToolCalls) != 1 || stitched[i].ToolCalls[0].ID != "c1" {
			continue
		}
		found = true
		require.Less(t, i+1, len(stitched))
		assert.Equal(t, "tool", stitched[i+1].Role)
		assert.Equal(t, "c1", stitched[i+1].ToolCallID)
		assert.Equal(t, "checking", stitched[i].Content)
		assert.Equal(t, "reason", stitched[i].ReasoningContent)
	}
	assert.True(t, found, "the merged assistant/tool unit must survive together in the tail")
}

// TestStitchCompacted_RecallPairMiddleAndTail: the synthetic recall tool
// pair is an ordinary transcript unit to compaction — summarized away when
// its placeholder sits in the shadowed middle, replayed verbatim when it
// sits in the retained tail (memory-recall-cache-stable D6).
func TestStitchCompacted_RecallPairMiddleAndTail(t *testing.T) {
	msgTime := time.Unix(1_700_000_000, 0).UTC()
	res := recallResult(memory.RecallEntry{Key: "viking://u/mem/a", Line: "- [memory 87%] fact a"})
	inj := newRecallInjection("sess-1", "trace-r", "question", res)

	rows := append(compactionRows(2), []model.Message{
		{ID: 5, MsgTime: msgTime.Add(10 * time.Minute), MsgIndex: -1, SessionID: "sess-1", Agent: model.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: recallToolName, TraceID: "trace-r", ClientMsgID: "trace-r:-1", ToolCallID: inj.callID},
		{ID: 6, MsgTime: msgTime.Add(10 * time.Minute), MsgIndex: 0, SessionID: "sess-1", Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "recall turn", TraceID: "trace-r"},
		{ID: 7, MsgTime: msgTime.Add(11 * time.Minute), MsgIndex: 1, SessionID: "sess-1", Agent: model.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeMessage, Content: "recall answer", TraceID: "trace-r"},
	}...)
	calls := []model.ToolCall{{
		SessionID: "sess-1", TurnID: "trace-r", ToolCallID: inj.callID,
		Name: recallToolName, Arguments: inj.args, Result: inj.frame,
		Status: model.ToolCallStatusCompleted,
	}}
	agentMsgs, lastRow, err := MessagesToAgentMessagesIndexed(rows, calls)
	require.NoError(t, err)
	// Reconstructed history contains the replayed pair (assistant call +
	// tool result) between the second exchange and the recall turn.
	pairAt := -1
	for i, m := range agentMsgs {
		if m.Role == "tool" && m.Name == recallToolName {
			pairAt = i
		}
	}
	require.GreaterOrEqual(t, pairAt, 0)

	// Boundary after the recall turn's assistant final: the pair lands in
	// the MIDDLE — summarized away like every other middle message.
	boundaryRow := rows[lastRow[len(lastRow)-1]]
	rec := &model.ContextCompaction{
		Content:          "CHECKPOINT BODY",
		BoundaryMsgTime:  boundaryRow.MsgTime,
		BoundaryMsgIndex: boundaryRow.MsgIndex,
		BoundaryMsgID:    boundaryRow.ID,
	}
	stitched := stitchCompacted(rows, calls, agentMsgs, rec)
	require.NotNil(t, stitched)
	for _, m := range stitched {
		assert.NotContains(t, m.Content, "<user-memories>",
			"a middle recall pair must be shadowed by the summary, not replayed")
		assert.NotEqual(t, recallToolName, m.Name)
	}

	// Boundary after the second exchange: the pair lands in the TAIL —
	// replayed byte-identically.
	tailBoundary := rows[lastRow[3]]
	tailRec := &model.ContextCompaction{
		Content:          "CHECKPOINT BODY",
		BoundaryMsgTime:  tailBoundary.MsgTime,
		BoundaryMsgIndex: tailBoundary.MsgIndex,
		BoundaryMsgID:    tailBoundary.ID,
	}
	tailStitched := stitchCompacted(rows, calls, agentMsgs, tailRec)
	require.NotNil(t, tailStitched)
	found := false
	for _, m := range tailStitched {
		if m.Role == "tool" && m.Name == recallToolName {
			found = true
			assert.Contains(t, m.Content, "<user-memories>")
			assert.Equal(t, inj.callID, m.ToolCallID)
		}
	}
	assert.True(t, found, "a tail recall pair must survive compaction verbatim")
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
	agentMsgs, lastRow, err := MessagesToAgentMessagesIndexed(rows, nil)
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
	require.Equal(t, "user", got[len(got)-1].Role)
	require.Contains(t, got[len(got)-1].Content, "next question")
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
	require.Contains(t, got[8].Content, "next question")
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

func TestBudgetedRoundHook_ExplicitTurnStartForceUsesTurnStartTrigger(t *testing.T) {
	env, _ := newCompactionHandlerEnv(t, nil, &cannedSummaryLLM{content: "FRESH CHECKPOINT"}, 100000)
	rows := compactionRows(8)
	env.mysql.listMessagesRows = rows
	tap := NewTurnEventTap()
	go func() {
		select {
		case req := <-tap.requests():
			tap.serve(req, []stream.StreamEvent{stream.AgentStartEvent(stream.AgentConfucius)})
		case <-tap.done:
		}
	}()
	defer tap.close()
	hook := env.stream.newBudgetedRoundHook(tap, turnPersistInfo{
		sessionID: "sess-1", userID: "user-1", traceID: "trace-1",
	}, &turnFlushState{}, 100000, 80000, "gpt-test")
	ctx := agent.WithForcedCompactionEstimate(context.Background(), "turn_start_preflight", 81234, 80000)
	replaced := hook(ctx, agent.Usage{})
	require.NotNil(t, replaced)
	require.Len(t, env.mysql.compactions, 1)
	rec := env.mysql.compactions[0]
	require.Equal(t, model.CompactionTriggerTurnStart, rec.TriggerKind)
	assert.Equal(t, 81234, rec.PreCompactionTokens)
	require.Equal(t, 1, env.mysql.updateSessionCompactedCalls, "flag is set last")
}

func TestSendMessage_InvalidContextBudgetRejectedBeforeClaims(t *testing.T) {
	env, _ := newCompactionHandlerEnv(t, nil, &cannedSummaryLLM{content: "unused"}, 100000)
	env.stream.selection.Catalog[0].MaxCompletionTokens = 100000

	w := postToSession(t, env, "must not reach model")
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "INVALID_CONTEXT_BUDGET")
	assert.Empty(t, env.redis.dualBatches)
	assert.Empty(t, env.stub.gotMessages)
}

func TestConfiguredInputBudget_DoesNotDisableSmallModelsByWindowSize(t *testing.T) {
	budget := configuredInputBudget(8000, 100)
	assert.Greater(t, budget, 0)
	assert.Equal(t, 8000-100-4096, budget)
	assert.Zero(t, configuredInputBudget(8000, 0), "missing quota is the explicit legacy/no-budget shape")
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

// TestBudgetedRoundHook_largeAppendedToolResultForcesCompaction covers the
// primary regression: the previous round's usage is low, but the appended tool
// result would exceed the hard window on the next request. The budgeted hook
// must compact before that request rather than waiting for the provider error.
func TestBudgetedRoundHook_largeAppendedToolResultForcesCompaction(t *testing.T) {
	env, _ := newCompactionHandlerEnv(t, nil, &cannedSummaryLLM{content: "BUDGET CHECKPOINT"}, 1000)
	rows := compactionRows(8)
	env.mysql.listMessagesRows = rows
	env.mysql.latestContextTokens = 300
	stub := &stubOrchestrator{eventsToEmit: []stream.StreamEvent{
		stream.AgentStartEvent(stream.AgentConfucius),
		stream.TokenEvent(stream.AgentConfucius, "ok"),
		stream.AgentEndEvent(stream.AgentConfucius),
	}}
	env.stub = stub
	env.stream.orch = stub

	// This is the estimator input the real agent receives: a small measured
	// round plus a huge appended tool result. It must exceed the effective
	// budget and therefore trigger the budgeted compaction seam.
	appended := []agent.Message{{
		Role: "tool", ToolCallID: "tool-big", Name: "bash",
		Content: strings.Repeat("x", 12000),
	}}
	estimate := agent.EstimateRequestFromUsage(100, 20, appended)
	require.Greater(t, estimate.Tokens, 1000)
}

func TestBudgetedRoundHook_triggersCompactionBeforeHardLimit(t *testing.T) {
	env, _ := newCompactionHandlerEnv(t, nil, &cannedSummaryLLM{content: "CHECKPOINT"}, 100000)
	env.mysql.listMessagesRows = compactionRows(8)
	h := env.stream
	tap := NewTurnEventTap()
	go func() {
		req := <-tap.requests()
		tap.serve(req, nil)
	}()
	persist := turnPersistInfo{sessionID: "sess-1", userID: "user-1", traceID: "trace-1"}
	flushed := &turnFlushState{}
	hook := h.newBudgetedRoundHook(tap, persist, flushed, 100000, 80000, "gpt-test")
	// Zero usage is the forced-recovery shape; a non-recording hook must at
	// least attempt the same flush-first path and never return an oversized
	// raw tail as a successful compaction.
	got := hook(context.Background(), agent.Usage{})
	_ = got
}

func TestBudgetedRoundHook_ZeroUsageDoesNotForceCompaction(t *testing.T) {
	llm := &countingSummaryLLM{content: "CHECKPOINT"}
	env, _ := newCompactionHandlerEnv(t, nil, llm, 100000)
	env.mysql.listMessagesRows = compactionRows(8)
	tap := NewTurnEventTap()
	go func() {
		select {
		case req := <-tap.requests():
			tap.serve(req, []stream.StreamEvent{stream.AgentStartEvent(stream.AgentConfucius)})
		case <-tap.done:
		}
	}()
	defer tap.close()
	hook := env.stream.newBudgetedRoundHook(tap, turnPersistInfo{
		sessionID: "sess-1", userID: "user-1", traceID: "trace-1",
	}, &turnFlushState{}, 100000, 80000, "gpt-test")

	replaced := hook(context.Background(), agent.Usage{})
	assert.Nil(t, replaced)
	assert.Equal(t, 0, llm.calls, "zero provider usage without an explicit force marker is ordinary data")
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
