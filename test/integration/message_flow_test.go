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
	"github.com/lush/blowball/internal/tool"
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

	// Wait for the single combined batch (user message + assistant events) to
	// land in every tier. The batch is saved AFTER the SSE response completes via
	// a detached context, so we poll.
	require.Eventually(t, func() bool {
		return len(env.mysqlFake.messagesFor(defaultSessionID)) == 4 // 1 user + 3 merged assistant events
	}, 2*time.Second, 10*time.Millisecond, "expected user + 3 merged assistant events in MySQL tier")

	// Redis tier: 4 messages cached under msgs:{session_id}.
	require.Eventually(t, func() bool {
		raws, err := env.redisSvc.GetMessages(context.Background(), defaultSessionID)
		return err == nil && len(raws) == 4
	}, 2*time.Second, 10*time.Millisecond, "expected 4 messages cached in Redis")

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
	require.Len(t, doc.Messages, 4, "FS session file must contain user + 3 merged assistant events")

	var first model.Message
	require.NoError(t, json.Unmarshal(doc.Messages[0], &first))
	assert.Equal(t, model.RoleUser, first.Role)
	assert.Equal(t, model.AgentUser, first.Agent)
	assert.Equal(t, model.EventTypeMessage, first.EventType)
	assert.Equal(t, 0, first.MsgIndex)
	assert.Equal(t, "hello", first.Content)

	// Assistant events follow in order: agent_start, merged token, agent_end.
	wantEvents := []struct {
		EventType string
		Agent     string
		Role      string
	}{
		{model.EventTypeAgentStart, stream.AgentConfuse, ""},
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
	require.Len(t, recovered, 4)
	assert.Equal(t, "hello", recovered[0].Content)
	assert.Equal(t, model.EventTypeAgentStart, recovered[1].EventType)
	assert.Equal(t, model.EventTypeAgentEnd, recovered[3].EventType)
}

// TestMessageFlow_OrchestratorFailure_PersistsNothing verifies that when the
// orchestrator returns an error, neither the user message nor any assistant
// event rows are written.
func TestMessageFlow_OrchestratorFailure_PersistsNothing(t *testing.T) {
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

	// No messages should be persisted when the orchestrator fails.
	require.Eventually(t, func() bool {
		return len(env.mysqlFake.messagesFor(defaultSessionID)) == 0
	}, 2*time.Second, 10*time.Millisecond, "expected zero messages in MySQL tier on orchestrator failure")

	msgs := env.mysqlFake.messagesFor(defaultSessionID)
	require.Empty(t, msgs, "expected no messages persisted on orchestrator failure")
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

// TestCreateSession_ExplicitCreation_ReturnsUUIDv7 verifies the new explicit
// session creation endpoint returns a server-generated UUID v7 session_id and
// persists it.
func TestCreateSession_ExplicitCreation_ReturnsUUIDv7(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())
	token := authToken(t, defaultUserID)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.SessionID, 36)
	assert.Equal(t, byte('7'), resp.SessionID[14])

	env.mysqlFake.mu.Lock()
	sess, ok := env.mysqlFake.sessions[resp.SessionID]
	env.mysqlFake.mu.Unlock()
	require.True(t, ok, "session must be persisted")
	assert.Equal(t, defaultUserID, sess.UserID)
}

// TestMessageFlow_SessionMustExist_404 verifies that posting a message to a
// non-existent session returns 404 instead of auto-creating it.
func TestMessageFlow_SessionMustExist_404(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())
	token := authToken(t, defaultUserID)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/non-existent-session/messages",
		strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	assert.NotEqual(t, "text/event-stream", w.Result().Header.Get("Content-Type"))
}

// TestGetSessionMessages_Pagination_ReturnsFullEventStream verifies that the
// messages endpoint returns every persisted event for a session with cursor
// pagination.
func TestGetSessionMessages_Pagination_ReturnsFullEventStream(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())
	token := authToken(t, defaultUserID)

	// First, create a session explicitly.
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
	createReq.Header.Set("Authorization", "Bearer "+token)
	createW := httptest.NewRecorder()
	env.engine.ServeHTTP(createW, createReq)
	require.Equal(t, http.StatusOK, createW.Code)
	var createResp struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(createW.Body.Bytes(), &createResp))

	// Seed some messages through the memory store (faster than running the
	// orchestrator for this test).
	base := time.Unix(1_700_000_000, 0).UTC()
	env.mysqlFake.mu.Lock()
	env.mysqlFake.messages[createResp.SessionID] = []model.Message{
		{ID: 1, SessionID: createResp.SessionID, MsgTime: base, MsgIndex: 0, Role: model.RoleUser, EventType: model.EventTypeMessage, Agent: model.AgentUser, Content: "hello"},
		{ID: 2, SessionID: createResp.SessionID, MsgTime: base.Add(time.Second), MsgIndex: 1, Role: model.RoleAssistant, EventType: model.EventTypeToken, Agent: stream.AgentConfuse, Content: "Hi"},
	}
	env.mysqlFake.mu.Unlock()

	// Fetch first page with page_size=1.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/sessions/"+createResp.SessionID+"/messages?page_size=1",
		nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var page1 struct {
		Messages      []model.Message `json:"messages"`
		NextPageToken string          `json:"next_page_token"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page1))
	require.Len(t, page1.Messages, 1)
	assert.Equal(t, "hello", page1.Messages[0].Content)
	assert.NotEmpty(t, page1.NextPageToken)

	// Fetch second page using the token.
	req2 := httptest.NewRequest(http.MethodGet,
		"/api/v1/sessions/"+createResp.SessionID+"/messages?page_size=1&page_token="+page1.NextPageToken,
		nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	w2 := httptest.NewRecorder()
	env.engine.ServeHTTP(w2, req2)

	require.Equal(t, http.StatusOK, w2.Code, "body: %s", w2.Body.String())
	var page2 struct {
		Messages      []model.Message `json:"messages"`
		NextPageToken string          `json:"next_page_token"`
	}
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &page2))
	require.Len(t, page2.Messages, 1)
	assert.Equal(t, "Hi", page2.Messages[0].Content)
	assert.Empty(t, page2.NextPageToken, "last page must have empty next_page_token")
}

// TestGetSessionMessages_WrongOwner_404 verifies that a user cannot read
// another user's session messages.
func TestGetSessionMessages_WrongOwner_404(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())
	ownerToken := authToken(t, "owner-user")

	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
	createReq.Header.Set("Authorization", "Bearer "+ownerToken)
	createW := httptest.NewRecorder()
	env.engine.ServeHTTP(createW, createReq)
	require.Equal(t, http.StatusOK, createW.Code)
	var createResp struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(createW.Body.Bytes(), &createResp))

	attackerToken := authToken(t, "attacker-user")
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/sessions/"+createResp.SessionID+"/messages",
		nil)
	req.Header.Set("Authorization", "Bearer "+attackerToken)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
}

// TestMessageFlow_TwoTurns_PromptContainsHistory sends two user messages in the
// same session and asserts that the second turn's LLM prompt contains the first
// turn's user message and assistant reply.
func TestMessageFlow_TwoTurns_PromptContainsHistory(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{tokens: []string{"first reply"}, content: "first reply", finishReason: "stop", usage: agent.Usage{TotalTokens: 3}},
		scriptedLLMResponse{content: "First Title", finishReason: "stop", usage: agent.Usage{TotalTokens: 2}},
		scriptedLLMResponse{tokens: []string{"second reply"}, content: "second reply", finishReason: "stop", usage: agent.Usage{TotalTokens: 3}},
	)
	env := newTestEnv(t, llm)
	token := authToken(t, defaultUserID)

	// First turn.
	w := env.postMessage(`{"content":"first message"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Wait for the title-generation goroutine to consume its LLM round.
	require.Eventually(t, func() bool {
		env.mysqlFake.mu.Lock()
		defer env.mysqlFake.mu.Unlock()
		return len(env.mysqlFake.titles[defaultSessionID].Title) > 0
	}, 2*time.Second, 10*time.Millisecond, "expected title to be generated")

	// Second turn.
	w = env.postMessage(`{"content":"second message"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	client := llm
	var secondTurn *agent.LLMRequest
	for i := range client.calls {
		msgs := client.calls[i].Messages
		if len(msgs) > 0 && msgs[len(msgs)-1].Role == "user" && msgs[len(msgs)-1].Content == "second message" {
			req := client.calls[i]
			secondTurn = &req
			break
		}
	}
	require.NotNil(t, secondTurn, "second turn LLM request not found")

	// The prompt should include the system prompt, first user message, first
	// assistant reply, and the current user message.
	var contents []string
	for _, m := range secondTurn.Messages {
		contents = append(contents, m.Content)
	}
	assert.Contains(t, contents, "first message")
	assert.Contains(t, contents, "first reply")
	assert.Contains(t, contents, "second message")
}

// TestMessageFlow_ToolCallMemory exercises a two-turn session where the first
// turn triggers a tool call. It asserts that the reconstructed history passed
// to the second turn contains both the tool call and its result.
func TestMessageFlow_ToolCallMemory(t *testing.T) {
	baseReg := tool.NewRegistry()
	require.NoError(t, baseReg.Register(
		&tool.ToolSpec{
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

	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"calling tool"},
			content:      "",
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID:       "tc-1",
				Function: agent.ToolCallFunction{Name: "echo", Arguments: `{"input":"hello"}`},
			}},
			usage: agent.Usage{TotalTokens: 3},
		},
		scriptedLLMResponse{content: "Tool Title", finishReason: "stop", usage: agent.Usage{TotalTokens: 2}},
		scriptedLLMResponse{tokens: []string{"result was hello"}, content: "result was hello", finishReason: "stop", usage: agent.Usage{TotalTokens: 4}},
		scriptedLLMResponse{tokens: []string{"I remember"}, content: "I remember", finishReason: "stop", usage: agent.Usage{TotalTokens: 3}},
	)

	env := newTestEnvWithRegistry(t, llm, baseReg, []string{"echo"})
	token := authToken(t, defaultUserID)

	// First turn: tool call + result.
	w := env.postMessage(`{"content":"say hello"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Wait for title generation to consume its round.
	require.Eventually(t, func() bool {
		env.mysqlFake.mu.Lock()
		defer env.mysqlFake.mu.Unlock()
		return len(env.mysqlFake.titles[defaultSessionID].Title) > 0
	}, 2*time.Second, 10*time.Millisecond, "expected title to be generated")

	// Second turn.
	w = env.postMessage(`{"content":"what did I ask"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	client := llm
	var secondTurn *agent.LLMRequest
	for i := range client.calls {
		msgs := client.calls[i].Messages
		if len(msgs) > 0 && msgs[len(msgs)-1].Role == "user" && msgs[len(msgs)-1].Content == "what did I ask" {
			req := client.calls[i]
			secondTurn = &req
			break
		}
	}
	require.NotNil(t, secondTurn, "second turn LLM request not found")

	var sawToolCall, sawToolResult bool
	for _, m := range secondTurn.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				if tc.ID == "tc-1" && tc.Function.Name == "echo" {
					sawToolCall = true
				}
			}
		}
		if m.Role == "tool" && m.ToolCallID == "tc-1" && m.Content == "hello" {
			sawToolResult = true
		}
	}
	assert.True(t, sawToolCall, "second turn prompt should contain the tool call")
	assert.True(t, sawToolResult, "second turn prompt should contain the tool result")
}
