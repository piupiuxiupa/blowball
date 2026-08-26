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
	users    []string                 // X-OpenViking-User per find call
	bodies   []map[string]any         // decoded bodies of every request
	findCode int                      // non-zero → Find fails (degradation path)
	batch    chan map[string]any      // signals a messages/batch arrival
	commit   chan map[string]any      // signals a commit arrival
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
					{"uri": "viking://user/memories/prefs", "abstract": "User runs fish shell on macOS.", "score": 0.87},
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

// TestMemoryRecall_InjectsEphemeralBlock: the recall block lands as a
// user-role message immediately before the new user message in the
// orchestrator's context, and NEVER in the persisted rows (ephemerality).
func TestMemoryRecall_InjectsEphemeralBlock(t *testing.T) {
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
	require.GreaterOrEqual(t, len(msgs), 2, "stub should receive the recall block plus the user message")
	last, secondLast := msgs[len(msgs)-1], msgs[len(msgs)-2]
	assert.Equal(t, "user", last.Role)
	assert.Equal(t, "which shell do I use?", last.Content)
	assert.Equal(t, "user", secondLast.Role)
	assert.Contains(t, secondLast.Content, "<user-memories>")
	assert.Contains(t, secondLast.Content, "fish shell on macOS")

	// Ephemeral: nothing persisted (Redis dual-write batches nor the MySQL
	// fallback) carries the recall frame.
	require.Eventually(t, func() bool { return len(env.redis.batches()) > 0 }, 2*time.Second, 20*time.Millisecond)
	for _, batch := range env.redis.batches() {
		for _, m := range batch {
			assert.NotContains(t, m.Content, "<user-memories>",
				"the recall block must never be persisted")
		}
	}
	for _, m := range env.mysql.appendedMessages() {
		assert.NotContains(t, m.Content, "<user-memories>")
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
	assert.Equal(t, "hello", msgs[len(msgs)-1].Content)
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
	assert.Equal(t, "remember fish shell", msgs[0].(map[string]any)["content"])
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
		assert.Equal(t, "another question", msgs[0].(map[string]any)["content"])
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
	assert.Equal(t, "plain turn", msgs[len(msgs)-1].Content)
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
		{Type: stream.EventToolCall, Agent: model.AgentConfucius, Content: "invoke_chongzhi"},
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
