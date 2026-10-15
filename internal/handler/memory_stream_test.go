package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// ovCapture is a tiny OpenViking fake for the handler seam: it answers Find
// with one canned memory and signals every session batch/commit arrival on
// channels so tests can await the fire-and-forget capture deterministically.
type ovCapture struct {
	mu       sync.Mutex
	users    []string            // X-OpenViking-User per find call
	bodies   []map[string]any    // decoded bodies of every request
	findCode int                 // non-zero → Find fails (degradation path)
	batch    chan map[string]any // signals a messages/batch arrival
	commit   chan map[string]any // signals a commit arrival
}

func newOVCapture() *ovCapture {
	return &ovCapture{batch: make(chan map[string]any, 8), commit: make(chan map[string]any, 8)}
}

func (o *ovCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	o.mu.Lock()
	o.users = append(o.users, r.Header.Get("X-OpenViking-User"))
	o.bodies = append(o.bodies, body)
	o.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/api/v1/search/find":
		if o.findCode != 0 {
			w.WriteHeader(o.findCode)
			_, _ = w.Write([]byte(`{"status":"error","error":{"code":"INTERNAL","message":"boom"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"result": map[string]any{
				"memories": []map[string]any{
					{"uri": "viking://user/user-1/memories/prefs", "abstract": "User runs fish shell on macOS.", "score": 0.87},
				},
			},
		})
	case strings.HasSuffix(r.URL.Path, "/messages/batch"):
		o.batch <- body
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "result": map[string]any{}})
	case strings.HasSuffix(r.URL.Path, "/commit"):
		o.commit <- body
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "result": map[string]any{}})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "result": map[string]any{}})
	}
}

// newMemoryHandlerEnv mirrors newCompactionHandlerEnv with the memory service
// wired over the OV fake. The stub records the messages the orchestrator
// received (the recall-injection assertion point).
func newMemoryHandlerEnv(t *testing.T, stub *stubOrchestrator, ovSrvURL string) *sessionHandlerTestEnv {
	t.Helper()
	mysql := &handlerFakeMySQL{getSessionByIDFound: &model.Session{SessionID: "sess-1", UserID: "user-1"}}
	redis := &handlerFakeRedis{}
	fs := newHandlerFakeFS()
	deps := sessionDeps(mysql, redis, fs)
	sessSvc := newSessionSvc(deps)
	msgSvc := newMessageSvc(deps)
	titleSvc := service.NewTitleService(nil, mysql, config.OpenAIConfig{TitleModel: "title-model"})
	memSvc := memory.NewService(config.MemoryConfig{
		Enabled:           true,
		BaseURL:           ovSrvURL,
		Account:           "blowball",
		RecallLimit:       6,
		RecallTokenBudget: 2000,
		RecallTimeout:     2 * time.Second,
		CaptureTimeout:    3 * time.Second,
		MaxCaptureBytes:   256 << 10,
	})
	require.NotNil(t, memSvc)

	streamH := NewMessageStreamHandler(sessSvc, msgSvc, titleSvc, nil, memSvc, stub, "/tmp/blowball-test-data", run.NewManager(run.NewMemStore(), run.NewRegistry()), testSelectionConfig(), 0)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-1")
		c.Next()
	})
	r.POST("/api/v1/sessions/:session_id/messages", streamH.SendMessage)
	return &sessionHandlerTestEnv{stream: streamH, mysql: mysql, redis: redis, fs: fs, stub: stub, engine: r}
}

// TestMemoryRecall_InjectsPersistedToolPair: the recall frame lands as a
// synthetic recall_memories tool pair immediately before the new user
// message in the orchestrator's context, and BOTH sides persist through the
// ordinary final-batch path — placeholder row (msg_index -1) ordered before
// the user row (msg_index 0), canonical tool_calls row completed with the
// frame as its result (memory-recall-cache-stable).
func TestMemoryRecall_InjectsPersistedToolPair(t *testing.T) {
	ov := newOVCapture()
	srv := httptest.NewServer(ov)
	defer srv.Close()

	stub := &stubOrchestrator{eventsToEmit: []stream.StreamEvent{
		stream.AgentStartEvent(stream.AgentConfucius),
		stream.TokenEvent(stream.AgentConfucius, "ok"),
		stream.AgentEndEvent(stream.AgentConfucius),
	}}
	env := newMemoryHandlerEnv(t, stub, srv.URL)
	w := postToSession(t, env, "which shell do I use?")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	msgs := stub.messages()
	require.GreaterOrEqual(t, len(msgs), 3, "stub should receive the recall pair plus the user message")
	last, toolMsg, callMsg := msgs[len(msgs)-1], msgs[len(msgs)-2], msgs[len(msgs)-3]
	assert.Equal(t, "user", last.Role)
	assert.Contains(t, last.Content, "which shell do I use?")
	assert.Equal(t, "tool", toolMsg.Role)
	assert.Equal(t, recallToolName, toolMsg.Name)
	assert.Contains(t, toolMsg.Content, "<user-memories>")
	assert.Contains(t, toolMsg.Content, "fish shell on macOS")
	require.Len(t, callMsg.ToolCalls, 1)
	assert.Equal(t, recallToolName, callMsg.ToolCalls[0].Function.Name)
	assert.Equal(t, toolMsg.ToolCallID, callMsg.ToolCalls[0].ID)
	var args recallArgs
	require.NoError(t, json.Unmarshal([]byte(callMsg.ToolCalls[0].Function.Arguments), &args))
	assert.Equal(t, "which shell do I use?", args.Query)
	assert.Equal(t, []string{"viking://user/user-1/memories/prefs"}, args.Entries)

	// Persisted: the send-time final batch carries the placeholder row
	// BEFORE the user row, and the tool_calls row carries the frame as its
	// completed result. (The user row itself rides the dual-write pipeline;
	// ordering across tiers is (msg_time, msg_index) with the placeholder
	// at -1.)
	batches, calls := env.mysql.finalBatchSnapshots()
	require.NotEmpty(t, batches)
	pairBatch := batches[0]
	require.Len(t, pairBatch, 1)
	ph := pairBatch[0]
	assert.Equal(t, model.RoleAssistant, ph.Role)
	assert.Equal(t, model.EventTypeToolCall, ph.EventType)
	assert.Equal(t, -1, ph.MsgIndex)
	assert.Equal(t, recallToolName, ph.Content)
	assert.Equal(t, "trace-1:-1", ph.ClientMsgID)
	require.Len(t, calls[0], 1)
	tc := calls[0][0]
	assert.Equal(t, ph.ToolCallID, tc.ToolCallID)
	assert.Equal(t, recallToolName, tc.Name)
	assert.Equal(t, model.ToolCallStatusCompleted, tc.Status)
	assert.Contains(t, tc.Result, "fish shell on macOS")
	assert.Equal(t, callMsg.ToolCalls[0].Function.Arguments, tc.Arguments,
		"in-memory arguments must equal the persisted arguments byte-for-byte (append-only replay)")

	// The user-row path still excludes the frame: every persisted MESSAGE
	// row carries user/assistant content only (the frame lives in
	// tool_calls.result), so the ordinary message stream stays clean.
	require.Eventually(t, func() bool { return len(env.redis.batches()) > 0 }, 2*time.Second, 20*time.Millisecond)
	for _, batch := range env.redis.batches() {
		for _, m := range batch {
			if m.EventType == model.EventTypeMessage {
				assert.NotContains(t, m.Content, "<user-memories>")
			}
		}
	}
	for _, m := range env.mysql.appendedMessages() {
		if m.EventType == model.EventTypeMessage {
			assert.NotContains(t, m.Content, "<user-memories>")
		}
	}
}

// TestMemoryRecall_FailureDegrades: an OpenViking failure skips injection —
// the turn proceeds normally with exactly the pre-capability message shape.
func TestMemoryRecall_FailureDegrades(t *testing.T) {
	ov := newOVCapture()
	ov.findCode = http.StatusInternalServerError
	srv := httptest.NewServer(ov)
	defer srv.Close()

	stub := &stubOrchestrator{eventsToEmit: []stream.StreamEvent{
		stream.TokenEvent(stream.AgentConfucius, "ok"),
	}}
	env := newMemoryHandlerEnv(t, stub, srv.URL)
	w := postToSession(t, env, "hello")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	msgs := stub.messages()
	require.NotEmpty(t, msgs)
	assert.Contains(t, msgs[len(msgs)-1].Content, "hello")
	for _, m := range msgs {
		assert.NotContains(t, m.Content, "<user-memories>")
	}
}

// TestMemoryCapture_SuccessAndErrorPaths: the fire-and-forget capture fires
// on BOTH terminal paths with the persisted pair (user content + top-level
// assistant tokens) and the per-user identity header.
func TestMemoryCapture_SuccessAndErrorPaths(t *testing.T) {
	ov := newOVCapture()
	srv := httptest.NewServer(ov)
	defer srv.Close()

	// Success path.
	stub := &stubOrchestrator{eventsToEmit: []stream.StreamEvent{
		stream.AgentStartEvent(stream.AgentConfucius),
		stream.TokenEvent(stream.AgentConfucius, "final answer"),
		stream.AgentEndEvent(stream.AgentConfucius),
	}}
	env := newMemoryHandlerEnv(t, stub, srv.URL)
	w := postToSession(t, env, "remember fish shell")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var batch map[string]any
	select {
	case batch = <-ov.batch:
	case <-time.After(3 * time.Second):
		t.Fatal("capture never reached the OV session batch endpoint")
	}
	select {
	case <-ov.commit:
	case <-time.After(3 * time.Second):
		t.Fatal("capture never committed the OV session")
	}
	msgs := batch["messages"].([]any)
	require.Len(t, msgs, 2)
	assert.Contains(t, msgs[0].(map[string]any)["content"], "remember fish shell")
	assert.Equal(t, "final answer", msgs[1].(map[string]any)["content"])
	for _, u := range ov.findUsers() {
		assert.Equal(t, "user-1", u, "every OV request must carry the turn's user identity")
	}

	// Error path (orchestrator fails after tokens streamed): the capture
	// still fires — the partial exchange is durable memory signal.
	ov2 := newOVCapture()
	srv2 := httptest.NewServer(ov2)
	defer srv2.Close()
	errStub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{
			stream.TokenEvent(stream.AgentConfucius, "partial output"),
		},
		returnErr: context.Canceled,
	}
	env2 := newMemoryHandlerEnv(t, errStub, srv2.URL)
	_ = postToSession(t, env2, "another question")
	select {
	case b := <-ov2.batch:
		msgs := b["messages"].([]any)
		require.Len(t, msgs, 2)
		assert.Contains(t, msgs[0].(map[string]any)["content"], "another question")
		assert.Equal(t, "partial output", msgs[1].(map[string]any)["content"])
	case <-time.After(3 * time.Second):
		t.Fatal("error-path capture never reached the OV session batch endpoint")
	}
}

// TestMemoryNilService_Regression: a nil service (disabled config) keeps the
// pre-capability message shape — no recall injection, no capture calls.
func TestMemoryNilService_Regression(t *testing.T) {
	ov := newOVCapture()
	srv := httptest.NewServer(ov)
	defer srv.Close()

	stub := &stubOrchestrator{eventsToEmit: []stream.StreamEvent{
		stream.TokenEvent(stream.AgentConfucius, "ok"),
	}}
	env := newMemoryHandlerEnv(t, stub, srv.URL)
	env.stream.memSvc = nil // simulate the disabled-config wiring
	w := postToSession(t, env, "plain turn")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	msgs := stub.messages()
	require.NotEmpty(t, msgs)
	assert.Contains(t, msgs[len(msgs)-1].Content, "plain turn")
	assert.Len(t, msgs, 1, "no recall block and no extra messages")

	select {
	case <-ov.batch:
		t.Fatal("capture must not fire with a nil memory service")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestTopLevelAssistantText: only run-id-less token events survive; reasoning,
// tool events, and sub-agent (run-tagged) tokens are excluded; runs join with
// a blank line.
func TestTopLevelAssistantText(t *testing.T) {
	subAgent := map[string]any{stream.MetaParentToolCallID: "call-1"}
	events := []stream.StreamEvent{
		{Type: stream.EventToken, Agent: model.AgentConfucius, Content: "first round"},
		{Type: stream.EventReasoning, Agent: model.AgentConfucius, Content: "thinking…"},
		{Type: stream.EventToolCall, Agent: model.AgentConfucius, Content: "spawn_subagent"},
		{Type: stream.EventToken, Agent: model.AgentChongzhi, Content: "subagent output", Meta: subAgent},
		{Type: stream.EventToken, Agent: model.AgentConfucius, Content: "second round"},
	}
	merged := MergeEvents(events)
	got := topLevelAssistantText(merged)
	assert.Equal(t, "first round\n\nsecond round", got)
}

// --- small accessors over the handler-test fakes (concurrency-safe views). ---

func (s *stubOrchestrator) messages() []agent.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.Message(nil), s.gotMessages...)
}

func (f *handlerFakeRedis) batches() [][]model.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]model.Message(nil), f.dualBatches...)
}

func (f *handlerFakeMySQL) appendedMessages() []model.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.Message(nil), f.appendMessagesArg...)
}

func (o *ovCapture) findUsers() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.users...)
}
