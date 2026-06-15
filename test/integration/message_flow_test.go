package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

// TestMessageFlow_DirectAnswer_PersistsAllTiers exercises the full happy path:
// a user POSTs a chat message, the orchestrator produces a direct (no tool
// calls) answer, the SSE response carries the spec event sequence, and the
// user message plus every assistant event are persisted to Redis, FS, and the
// in-memory MySQL tier. The done event's Meta.usage must be non-nil.
//
// Components on the critical path:
//   - gin engine + middleware.TraceMiddleware + middleware.AuthMiddleware
//   - handler.RegisterRoutes + handler.SessionHandler.SendMessage
//   - service.SessionService (EnsureSession + SaveMessage/SaveMessagesBatch, three-tier write)
//   - service.MessageService (RecoverMessages, Redis hit on the assistant turn)
//   - real fs.Store, real redis.Store (miniredis), in-memory MySQL fake
//   - agent.NewOrchestrator + handler.NewOrchestratorAdapter
//   - agent.Confuse direct-answer loop (no tool calls)
//   - stream.Hub + stream.WriteSSE
func TestMessageFlow_DirectAnswer_PersistsAllTiers(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"Hello, ", "world!"},
			content:      "Hello, world!",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		},
		// TitleService is wired with the same client; it will pull the next
		// response when it fires asynchronously on the first turn. We give it
		// a harmless title round so the queue does not run dry.
		scriptedLLMResponse{
			content:      "Greeting",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	)
	env := newTestEnv(t, llm)

	token := authToken(t, defaultUserID)
	w := env.postMessage(`{"content":"hello"}`, token)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// SSE Content-Type must be set before the first write.
	require.Equal(t, "text/event-stream", w.Result().Header.Get("Content-Type"))

	types, payloads := parseSSEBody(t, w.Body.String())
	require.NotEmpty(t, types)

	// The spec mandates this ordering for a direct-answer turn.
	requireEventSubsequence(t, types, []string{
		stream.EventAgentStart,
		stream.EventToken,
		stream.EventAgentEnd,
		stream.EventDone,
	})

	// At least one token event came from Confuse.
	var confuseTokenSeen bool
	for _, p := range payloads {
		if p["type"] == stream.EventToken && p["agent"] == stream.AgentConfuse {
			confuseTokenSeen = true
		}
	}
	assert.True(t, confuseTokenSeen, "expected a Confuse token event")

	// The done event must carry a non-nil usage map.
	var donePayload map[string]any
	for _, p := range payloads {
		if p["type"] == stream.EventDone {
			donePayload = p
		}
	}
	require.NotNil(t, donePayload, "expected a done event")
	usageRaw, ok := donePayload["meta"].(map[string]any)["usage"]
	require.True(t, ok, "done event must include meta.usage; got %v", donePayload["meta"])
	usage, ok := usageRaw.(map[string]any)
	require.True(t, ok, "meta.usage must be an object; got %T", usageRaw)
	assert.NotNil(t, usage["total_tokens"], "meta.usage.total_tokens must be present")
	assert.Greater(t, usage["total_tokens"], float64(0), "meta.usage.total_tokens must be > 0")

	// The session row must exist in the MySQL tier.
	env.mysqlFake.mu.Lock()
	_, ok = env.mysqlFake.sessions[defaultSessionID]
	env.mysqlFake.mu.Unlock()
	assert.True(t, ok, "session must be persisted to MySQL tier")

	// Wait for the user message + assistant event batch to land in every tier.
	// The assistant batch is saved AFTER the SSE response completes via a
	// detached context, so we poll.
	require.Eventually(t, func() bool {
		return len(env.mysqlFake.messagesFor(defaultSessionID)) == 5 // 1 user + 4 assistant events
	}, 2*time.Second, 10*time.Millisecond, "expected user + 4 assistant events in MySQL tier")

	// Redis tier: 5 messages cached under msgs:{session_id}.
	require.Eventually(t, func() bool {
		raws, err := env.redisSvc.GetMessages(context.Background(), defaultSessionID)
		return err == nil && len(raws) == 5
	}, 2*time.Second, 10*time.Millisecond, "expected 5 messages cached in Redis")

	// FS tier: the session file must contain all messages in order.
	sessionFile := filepath.Join(env.dataDir, defaultUserID, "sessions", defaultSessionID+".json")
	require.Eventually(t, func() bool {
		_, err := os.Stat(sessionFile)
		return err == nil
	}, 2*time.Second, 10*time.Millisecond, "session file must exist on disk")

	data, err := os.ReadFile(sessionFile)
	require.NoError(t, err)

	var doc struct {
		SessionID string            `json:"session_id"`
		Messages  []json.RawMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))
	require.Len(t, doc.Messages, 5, "FS session file must contain user + 4 assistant events")

	var first model.Message
	require.NoError(t, json.Unmarshal(doc.Messages[0], &first))
	assert.Equal(t, model.RoleUser, first.Role)
	assert.Equal(t, model.AgentUser, first.Agent)
	assert.Equal(t, model.EventTypeMessage, first.EventType)
	assert.Equal(t, 0, first.MsgIndex)
	assert.Equal(t, "hello", first.Content)

	// Assistant events follow in order: agent_start, token, token, agent_end.
	wantEvents := []struct {
		EventType string
		Agent     string
		Role      string
	}{
		{model.EventTypeAgentStart, stream.AgentConfuse, ""},
		{model.EventTypeToken, stream.AgentConfuse, model.RoleAssistant},
		{model.EventTypeToken, stream.AgentConfuse, model.RoleAssistant},
		{model.EventTypeAgentEnd, stream.AgentConfuse, ""},
	}
	for i, want := range wantEvents {
		var m model.Message
		require.NoError(t, json.Unmarshal(doc.Messages[i+1], &m))
		assert.Equal(t, want.EventType, m.EventType, "assistant event %d", i)
		assert.Equal(t, want.Agent, m.Agent, "assistant event %d", i)
		assert.Equal(t, want.Role, m.Role, "assistant event %d", i)
		assert.Equal(t, i+1, m.MsgIndex, "assistant event %d", i)
	}

	// RecoverMessages through the public service API must return the same
	// ordered stream (Redis hit fast-path).
	recovered, err := env.msgSvc.RecoverMessages(context.Background(), defaultUserID, defaultSessionID)
	require.NoError(t, err)
	require.Len(t, recovered, 5)
	assert.Equal(t, "hello", recovered[0].Content)
	assert.Equal(t, model.EventTypeAgentStart, recovered[1].EventType)
	assert.Equal(t, model.EventTypeAgentEnd, recovered[4].EventType)
}

// TestMessageFlow_OrchestratorFailure_PersistsOnlyUserMessage verifies that
// when the orchestrator returns an error, only the user message survives in
// MySQL and no assistant event rows are written.
func TestMessageFlow_OrchestratorFailure_PersistsOnlyUserMessage(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"Hello"},
			content:      "Hello",
			finishReason: "stop",
			usage:        agent.Usage{TotalTokens: 1},
			err:          assert.AnError,
		},
	)
	env := newTestEnv(t, llm)

	token := authToken(t, defaultUserID)
	w := env.postMessage(`{"content":"hello"}`, token)

	// The handler returns 200 because the SSE stream starts before the error
	// is observed; the stream simply terminates early.
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Only the user message should be persisted.
	require.Eventually(t, func() bool {
		return len(env.mysqlFake.messagesFor(defaultSessionID)) == 1
	}, 2*time.Second, 10*time.Millisecond, "expected exactly one user message in MySQL tier")

	msgs := env.mysqlFake.messagesFor(defaultSessionID)
	require.Len(t, msgs, 1)
	assert.Equal(t, model.AgentUser, msgs[0].Agent)
	assert.Equal(t, model.EventTypeMessage, msgs[0].EventType)
	assert.Equal(t, model.RoleUser, msgs[0].Role)
	assert.Equal(t, 0, msgs[0].MsgIndex)
}

// TestMessageFlow_Unauthenticated_401 verifies that the real AuthMiddleware is
// in front of the SSE route: no Bearer token yields 401, not a stream.
func TestMessageFlow_Unauthenticated_401(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/"+defaultSessionID+"/messages",
		strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.NotEqual(t, "text/event-stream", w.Result().Header.Get("Content-Type"))
}
