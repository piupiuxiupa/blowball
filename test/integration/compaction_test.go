package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/handler"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/msgflush"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/store/fs"
	redisstore "github.com/lush/blowball/internal/store/redis"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/skill"
)

// seedHistory inserts n alternating user/assistant turns directly into the
// fake MySQL tier, mirroring what prior real turns would have left behind.
// Rows are inserted in (msg_time, msg_index, id) order so recovery,
// reconstruction, and pagination behave like production.
func seedHistory(t *testing.T, m *memoryMySQL, sessionID string, n int) {
	t.Helper()
	base := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < n; i++ {
		m.nextID++
		userID := m.nextID
		m.messages[sessionID] = append(m.messages[sessionID], model.Message{
			ID:        userID,
			SessionID: sessionID,
			MsgTime:   base.Add(time.Duration(2*i) * time.Minute),
			MsgIndex:  0,
			Agent:     model.AgentUser,
			Role:      model.RoleUser,
			EventType: model.EventTypeMessage,
			Content:   fmt.Sprintf("seed user %d", i),
			TraceID:   fmt.Sprintf("seed-trace-%d", i),
		})
		m.nextID++
		replyID := m.nextID
		m.messages[sessionID] = append(m.messages[sessionID], model.Message{
			ID:        replyID,
			SessionID: sessionID,
			MsgTime:   base.Add(time.Duration(2*i+1) * time.Minute),
			MsgIndex:  1,
			Agent:     model.AgentConfucius,
			Role:      model.RoleAssistant,
			EventType: model.EventTypeToken,
			Content:   fmt.Sprintf("seed reply %d", i),
			TraceID:   fmt.Sprintf("seed-trace-%d", i),
		})
	}
}

// newCompactionTestEnv mirrors newTestEnvWithRegistry but wires a real
// CompactionService and a single-entry model catalog whose window is the
// given maxContextTokens into the streaming handler, so mid-turn and
// turn-start compaction run end-to-end through the real orchestrator,
// adapter, event tap, and persistence path (model-effort-v2: the threshold
// follows the turn-resolved catalog entry's window).
func newCompactionTestEnv(t *testing.T, llm agent.LLMClient, baseReg *tool.Registry, confuciusTools []string, maxContextTokens int) *testEnv {
	t.Helper()

	// Register goleak FIRST so it runs LAST in the LIFO cleanup chain.
	t.Cleanup(func() { goleak.VerifyNone(t) })

	dataDir := t.TempDir()

	fsSvc, err := fs.New(dataDir)
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	redisSvc, err := redisstore.New(mr.Addr(), "", 0, time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisSvc.Close() })

	mysqlFake := newMemoryMySQL()
	require.NoError(t, mysqlFake.CreateSession(context.Background(), model.Session{
		SessionID: defaultSessionID,
		UserID:    defaultUserID,
		TraceID:   "seed-trace",
	}))

	deps := service.SessionDeps{
		MySQL: mysqlFake,
		Redis: redisSvc,
		FS:    fsSvc,
		DrainMessageQueue: func(ctx context.Context) error {
			return msgflush.Drain(ctx, redisSvc, mysqlFake)
		},
	}
	sessSvc := service.NewSessionService(deps)
	msgSvc := service.NewMessageService(deps, sessSvc.SaveMessage)
	titleSvc := service.NewTitleService(llm, mysqlFake, config.OpenAIConfig{TitleModel: "title-model"})

	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{
			APIKey: "test",
			Models: []config.ModelCatalogEntry{
				{Name: "gpt-test", MaxContextTokens: maxContextTokens, Thinking: false},
			},
		},
		JWT:    config.JWTConfig{Secret: integrationTestSecret, Expire: "1h"},
		Agents: agentConfigWithTools(confuciusTools),
	}
	orch, err := agent.NewOrchestrator(llm, cfg, baseReg, nil, skill.NewLoader("", nil), nil, mysqlFake)
	require.NoError(t, err)

	compSvc := service.NewCompactionService(deps, llm, "gpt-test", maxContextTokens)

	sessH := handler.NewSessionHandler(sessSvc, titleSvc, redisSvc.RunStore())
	runMgr := run.NewManager(redisSvc.RunStore(), run.NewRegistry())
	turnRunH := handler.NewTurnRunHandler(runMgr)
	streamH := handler.NewMessageStreamHandler(sessSvc, msgSvc, titleSvc, compSvc, nil, handler.NewOrchestratorAdapter(orch), dataDir, runMgr, handler.NewModelSelectionConfig(cfg), 0)
	wsH := handler.NewWorkspaceHandler(fsSvc, 1<<20, handler.OnlyOfficeSettings{})
	mcpH := handler.NewMCPHandler(baseReg, nil, fsSvc.UserWorkspace)
	skillH := handler.NewSkillHandler(fsSvc, nil)

	r := gin.New()
	r.Use(middleware.TraceMiddleware())
	handler.RegisterRoutes(r, handler.RouteDeps{
		AuthMW:                 middleware.AuthMiddleware(integrationTestSecret),
		QueryTokenAuthMW:       middleware.QueryTokenAuthMiddleware(integrationTestSecret),
		Login:                  func(*gin.Context) {},
		SessionList:            sessH.ListSessions,
		SessionCreate:          sessH.CreateSession,
		SessionMessages:        sessH.GetSessionMessages,
		SubAgentRuns:           handler.NewSubAgentHandler(service.NewSubAgentService(mysqlFake)).ListRuns,
		SubAgentRunDetail:      handler.NewSubAgentHandler(service.NewSubAgentService(mysqlFake)).GetRun,
		SendMessage:            streamH.SendMessage,
		TurnCancel:             turnRunH.CancelTurn,
		TurnEvents:             turnRunH.TurnEvents,
		SessionDelete:          sessH.DeleteSession,
		SessionUpdateTitle:     sessH.UpdateTitle,
		WorkspaceList:          wsH.List,
		WorkspaceUpload:        wsH.Upload,
		WorkspaceDownload:      wsH.Download,
		WorkspaceTokenDownload: wsH.TokenDownload,
		WorkspaceContent:       wsH.Content,
		WorkspaceWriteContent:  wsH.WriteContent,
		WorkspaceDelete:        wsH.Delete,
		WorkspaceRename:        wsH.Rename,
		WorkspaceCreate:        wsH.Create,
		MCPTools:               mcpH.Tools,
		SkillsList:             skillH.List,
	})

	return &testEnv{
		t:         t,
		engine:    r,
		dataDir:   dataDir,
		fsSvc:     fsSvc,
		redisSvc:  redisSvc,
		miniRedis: mr,
		mysqlFake: mysqlFake,
		llm:       llm,
		sessSvc:   sessSvc,
		msgSvc:    msgSvc,
	}
}

// echoTool returns a registry carrying one trivial echo tool.
func echoTool(t *testing.T) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry()
	require.NoError(t, reg.Register(&tool.ToolSpec{
		Name:           "echo",
		Description:    "echo the input",
		ParametersJSON: json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Input string `json:"input"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			return a.Input, nil
		},
	}))
	return reg
}

// getMessages fetches the display-path history through the real endpoint.
func getMessages(t *testing.T, e *testEnv, token string) []model.Message {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/sessions/"+defaultSessionID+"/messages?order=asc&page_size=200", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Messages []model.Message `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Messages
}

// requestAfter finds the first LLM request whose system prompt is the
// compaction checkpoint instruction (the summary call).
func findSummaryRequest(calls []agent.LLMRequest) *agent.LLMRequest {
	for i := range calls {
		if len(calls[i].Messages) > 0 && calls[i].Messages[0].Role == "system" &&
			strings.Contains(calls[i].Messages[0].Content, "checkpoint") {
			return &calls[i]
		}
	}
	return nil
}

// TestCompaction_MidTurnTrigger_EndToEnd covers the full mid-turn flow under
// a fake LLM: threshold reached after a tool round → flush-first persistence
// → compaction record + flag + cache → the SAME turn continues on the
// rewritten context → turn-end save persists only the suffix → no duplicate
// rows anywhere → the display path returns the complete history with no
// summary rows.
func TestCompaction_MidTurnTrigger_EndToEnd(t *testing.T) {
	llm := newScriptedLLMClient(
		// Round 1: dispatch a tool; the returned usage (900+10) crosses the
		// 80% threshold of max_context_tokens=1000, so the between-rounds
		// hook fires after the tool result.
		scriptedLLMResponse{
			tokens:       []string{"checking"},
			content:      "checking",
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID:       "tc-1",
				Function: agent.ToolCallFunction{Name: "echo", Arguments: `{"input":"hello"}`},
			}},
			usage: agent.Usage{PromptTokens: 900, CompletionTokens: 10},
		},
		// The hook's summary call consumes the next scripted response.
		scriptedLLMResponse{content: "CHECKPOINT: full task history", finishReason: "stop", usage: agent.Usage{PromptTokens: 60, CompletionTokens: 8}},
		// Round 2 runs against the rewritten (stitched) context.
		scriptedLLMResponse{tokens: []string{"all done"}, content: "all done", finishReason: "stop", usage: agent.Usage{PromptTokens: 200, CompletionTokens: 20}},
	)

	env := newCompactionTestEnv(t, llm, echoTool(t), []string{"echo"}, 1000)
	token := authToken(t, defaultUserID)
	seedHistory(t, env.mysqlFake, defaultSessionID, 8) // 8 prior user/assistant pairs

	w := env.postMessage(`{"content":"current task"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// One mid_turn record exists, the flag is set, and the summary went
	// through the shared LLM client.
	recs := env.mysqlFake.compactionsFor(defaultSessionID)
	require.Len(t, recs, 1, "expected exactly one compaction record")
	rec := recs[0]
	assert.Equal(t, model.CompactionTriggerMidTurn, rec.TriggerKind)
	assert.Equal(t, 910, rec.TriggerTokens)
	assert.Equal(t, "CHECKPOINT: full task history", rec.Content)
	assert.True(t, env.mysqlFake.isCompacted(defaultSessionID), "session flag must be set")
	summaryReq := findSummaryRequest(llm.requests())
	require.NotNil(t, summaryReq, "the compaction summary call must flow through the LLM client")

	// The continued round ran on the stitched context: first message verbatim,
	// framed summary as a user message, no shadowed middle content. The
	// rewritten round legitimately ENDS with the retained tool result, so scan
	// every request for the summary frame rather than filtering by last role.
	var round2 *agent.LLMRequest
	calls := llm.requests()
	for i := range calls {
		if calls[i].Messages[0].Role == "system" && strings.Contains(calls[i].Messages[0].Content, "checkpoint") {
			continue // the summary call itself
		}
		for _, m := range calls[i].Messages {
			if strings.Contains(m.Content, "<compacted-summary>") {
				round2 = &calls[i]
			}
		}
	}
	require.NotNil(t, round2, "the post-compaction round must carry the framed summary")
	var sawFirst, sawShadowed bool
	for _, m := range round2.Messages {
		if m.Content == "seed user 0" {
			sawFirst = true
		}
		if m.Content == "seed user 3" || m.Content == "seed reply 3" {
			sawShadowed = true
		}
	}
	assert.True(t, sawFirst, "the first message must survive verbatim")
	assert.False(t, sawShadowed, "shadowed middle content must not reach the model")

	// Persistence: flush + turn-end suffix produced the turn rows exactly
	// once. The turn yields the user message + 6 merged events
	// (agent_start, token, tool_call, tool_result, final token, agent_end).
	env.waitForPersistedTurn(t, defaultSessionID, 16+7)
	rows := env.mysqlFake.messagesFor(defaultSessionID)
	require.Len(t, rows, 16+7, "seeded history + this turn, with no duplicates from flush + turn-end save")
	ids := map[string]int{}
	for _, r := range rows {
		if r.ClientMsgID != "" {
			ids[r.ClientMsgID]++
		}
	}
	for id, n := range ids {
		require.Equal(t, 1, n, "client_msg_id %s appeared %d times", id, n)
	}
	// Every row of THIS turn carries a deterministic id ({trace}:{index}).
	for _, r := range rows[len(rows)-7:] {
		require.NotEmpty(t, r.ClientMsgID)
		require.Contains(t, r.ClientMsgID, ":")
	}

	// Display path: complete history, no summary row, order preserved.
	display := getMessages(t, env, token)
	require.Len(t, display, 16+7)
	assert.Equal(t, "current task", display[len(display)-7].Content)
	for _, m := range display {
		assert.NotContains(t, m.Content, "CHECKPOINT", "summary text must never become a message row")
	}
}

// TestCompaction_SubsequentTurnStitches: after a mid-turn compaction, the
// NEXT turn's model context is stitched from the record (first + summary +
// tail), while the display path keeps returning everything.
func TestCompaction_SubsequentTurnStitches(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			content:      "checking",
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID:       "tc-1",
				Function: agent.ToolCallFunction{Name: "echo", Arguments: `{"input":"x"}`},
			}},
			usage: agent.Usage{PromptTokens: 900, CompletionTokens: 5},
		},
		scriptedLLMResponse{content: "CHECKPOINT ONE", finishReason: "stop"},
		scriptedLLMResponse{content: "wrapped up", finishReason: "stop", usage: agent.Usage{PromptTokens: 150, CompletionTokens: 10}},
		// Turn 2: no tools, ends under the threshold.
		scriptedLLMResponse{content: "plain answer", finishReason: "stop", usage: agent.Usage{PromptTokens: 120, CompletionTokens: 10}},
	)

	env := newCompactionTestEnv(t, llm, echoTool(t), []string{"echo"}, 1000)
	token := authToken(t, defaultUserID)
	seedHistory(t, env.mysqlFake, defaultSessionID, 8)

	w := env.postMessage(`{"content":"current task"}`, token)
	require.Equal(t, http.StatusOK, w.Code)
	env.waitForPersistedTurn(t, defaultSessionID, 16+5)

	w = env.postMessage(`{"content":"follow up"}`, token)
	require.Equal(t, http.StatusOK, w.Code)
	env.waitForPersistedTurn(t, defaultSessionID, 16+5+3)

	// Turn 2's request: first + framed summary + tail + "follow up".
	var turn2 *agent.LLMRequest
	for _, req := range llm.requests() {
		for _, m := range req.Messages {
			if m.Role == "user" && m.Content == "follow up" {
				turn2 = &req
			}
		}
	}
	require.NotNil(t, turn2, "turn 2 request not found")
	require.Equal(t, "seed user 0", turn2.Messages[1].Content, "context = system + first + ...")
	assert.Contains(t, turn2.Messages[2].Content, "CHECKPOINT ONE")
	assert.Contains(t, turn2.Messages[2].Content, "<compacted-summary>")
	for _, m := range turn2.Messages {
		assert.NotEqual(t, "seed user 3", m.Content, "shadowed middle must not be sent")
		assert.NotEqual(t, "seed reply 3", m.Content, "shadowed middle must not be sent")
	}
	assert.Equal(t, "follow up", turn2.Messages[len(turn2.Messages)-1].Content)

	// Display path still complete.
	display := getMessages(t, env, token)
	require.Len(t, display, 16+5+3)
}

// TestCompaction_RecompactionMergesPrior (task 6.5): a second trigger feeds
// the prior summary plus the newly shadowed span into one call, inserts a
// second record (latest-wins), and the msgs cache stays duplicate-free.
func TestCompaction_RecompactionMergesPrior(t *testing.T) {
	llm := newScriptedLLMClient(
		// Turn 1: mid-turn compaction #1.
		scriptedLLMResponse{
			content:      "checking",
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID:       "tc-1",
				Function: agent.ToolCallFunction{Name: "echo", Arguments: `{"input":"x"}`},
			}},
			usage: agent.Usage{PromptTokens: 900, CompletionTokens: 5},
		},
		scriptedLLMResponse{content: "CHECKPOINT ONE", finishReason: "stop"},
		scriptedLLMResponse{content: "wrapped up", finishReason: "stop", usage: agent.Usage{PromptTokens: 150, CompletionTokens: 5}},
		// Turn 2: crosses the threshold again → re-compaction #2 (merge).
		scriptedLLMResponse{
			content:      "checking again",
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID:       "tc-2",
				Function: agent.ToolCallFunction{Name: "echo", Arguments: `{"input":"y"}`},
			}},
			usage: agent.Usage{PromptTokens: 950, CompletionTokens: 5},
		},
		scriptedLLMResponse{content: "CONSOLIDATED CHECKPOINT", finishReason: "stop"},
		scriptedLLMResponse{content: "done again", finishReason: "stop", usage: agent.Usage{PromptTokens: 150, CompletionTokens: 5}},
		// Turn 3: verifies stitching reads the NEWEST record.
		scriptedLLMResponse{content: "final answer", finishReason: "stop", usage: agent.Usage{PromptTokens: 100, CompletionTokens: 5}},
	)

	env := newCompactionTestEnv(t, llm, echoTool(t), []string{"echo"}, 1000)
	token := authToken(t, defaultUserID)
	seedHistory(t, env.mysqlFake, defaultSessionID, 8)

	w := env.postMessage(`{"content":"current task"}`, token)
	require.Equal(t, http.StatusOK, w.Code)
	env.waitForPersistedTurn(t, defaultSessionID, 16+5)

	w = env.postMessage(`{"content":"second task"}`, token)
	require.Equal(t, http.StatusOK, w.Code)
	env.waitForPersistedTurn(t, defaultSessionID, 16+5+5)

	// Two records; the newest is authoritative.
	recs := env.mysqlFake.compactionsFor(defaultSessionID)
	require.Len(t, recs, 2, "re-compaction must append, not update")
	latest, err := env.mysqlFake.LatestCompaction(context.Background(), defaultSessionID)
	require.NoError(t, err)
	assert.Equal(t, "CONSOLIDATED CHECKPOINT", latest.Content, "latest-wins")
	assert.Greater(t, latest.BoundaryMsgID, recs[0].BoundaryMsgID, "boundary moves forward")

	// The second summary input merged the prior checkpoint.
	var secondSummary *agent.LLMRequest
	summaryReqs := 0
	for i := range llm.requests() {
		if len(llm.requests()[i].Messages) > 0 && strings.Contains(llm.requests()[i].Messages[0].Content, "checkpoint") {
			summaryReqs++
			secondSummary = &llm.requests()[i]
		}
	}
	require.Equal(t, 2, summaryReqs, "exactly two summary calls")
	require.Contains(t, secondSummary.Messages[1].Content, "PRIOR CHECKPOINT")
	require.Contains(t, secondSummary.Messages[1].Content, "CHECKPOINT ONE")

	// Turn 3 stitches from the consolidated record.
	w = env.postMessage(`{"content":"third task"}`, token)
	require.Equal(t, http.StatusOK, w.Code)
	env.waitForPersistedTurn(t, defaultSessionID, 16+5+5+3)
	var turn3 *agent.LLMRequest
	for _, req := range llm.requests() {
		for _, m := range req.Messages {
			if m.Role == "user" && m.Content == "third task" {
				turn3 = &req
			}
		}
	}
	require.NotNil(t, turn3)
	var sawConsolidated bool
	for _, m := range turn3.Messages {
		if strings.Contains(m.Content, "CONSOLIDATED CHECKPOINT") {
			sawConsolidated = true
		}
		assert.NotContains(t, m.Content, "CHECKPOINT ONE", "the superseded checkpoint must not be sent")
	}
	assert.True(t, sawConsolidated)

	// msgs:{sid} carries every row exactly once (no flush duplicates).
	raws, err := env.redisSvc.GetMessages(context.Background(), defaultSessionID)
	require.NoError(t, err)
	seen := map[string]int{}
	for _, raw := range raws {
		var m model.Message
		require.NoError(t, json.Unmarshal(raw, &m))
		if m.ClientMsgID == "" {
			continue // seeded legacy rows carry no id
		}
		seen[m.ClientMsgID]++
	}
	for id, n := range seen {
		assert.Equal(t, 1, n, "msgs cache duplicate for client_msg_id %s", id)
	}
	assert.Len(t, raws, 16+5+5+3)

	// Display path unaffected: full history, no checkpoints.
	display := getMessages(t, env, token)
	require.Len(t, display, 16+5+5+3)
	for _, m := range display {
		assert.NotContains(t, m.Content, "CHECKPOINT")
	}
}

// TestCompaction_DisabledByDefault: with max_context_tokens unset the whole
// capability is inert — over-threshold usage compacts nothing and the full
// history keeps flowing (the zero-behavior-change guarantee).
func TestCompaction_DisabledByDefault(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			content:      "checking",
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID:       "tc-1",
				Function: agent.ToolCallFunction{Name: "echo", Arguments: `{"input":"x"}`},
			}},
			usage: agent.Usage{PromptTokens: 9_000_000, CompletionTokens: 5},
		},
		scriptedLLMResponse{content: "done", finishReason: "stop", usage: agent.Usage{PromptTokens: 10, CompletionTokens: 5}},
	)

	env := newCompactionTestEnv(t, llm, echoTool(t), []string{"echo"}, 0)
	token := authToken(t, defaultUserID)
	seedHistory(t, env.mysqlFake, defaultSessionID, 8)

	w := env.postMessage(`{"content":"current task"}`, token)
	require.Equal(t, http.StatusOK, w.Code)
	env.waitForPersistedTurn(t, defaultSessionID, 16+5)

	assert.Empty(t, env.mysqlFake.compactionsFor(defaultSessionID), "disabled compaction must write no records")
	assert.False(t, env.mysqlFake.isCompacted(defaultSessionID))

	// The tool round's request carried the full history.
	var toolRound *agent.LLMRequest
	for _, req := range llm.requests() {
		if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Content == "current task" {
			toolRound = &req
		}
	}
	require.NotNil(t, toolRound)
	assert.Equal(t, "seed user 0", toolRound.Messages[1].Content)
	assert.Equal(t, "seed reply 7", toolRound.Messages[len(toolRound.Messages)-2].Content)
}
