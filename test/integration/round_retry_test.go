package integration

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

// TestRoundRetry_TransientFailureRecoveredEndToEnd verifies llm-round-retry
// end to end: Confucius's first stream breaks mid-flight with a transient
// error (the SDK no longer covers a stream that already started), the round
// retry re-issues the identical request, the turn completes normally, the SSE
// stream carries the retry marker, the marker is persisted as an agent_error
// row, and turn_usage reflects BOTH attempts' spend (real-spend accounting).
func TestRoundRetry_TransientFailureRecoveredEndToEnd(t *testing.T) {
	llm := newScriptedLLMClient(
		// Attempt 1: mid-stream 503 — transient, no content streamed.
		scriptedLLMResponse{
			err:   errors.New("confucius: stream chat: 503 Service Unavailable"),
			usage: agent.Usage{PromptTokens: 8, CompletionTokens: 4, TotalTokens: 12},
		},
		// Attempt 2 (the retry): the real answer.
		scriptedLLMResponse{
			tokens:       []string{"Hello"},
			content:      "Hello",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		},
	)

	agents := agentConfig()
	// Round-level retry on with a near-zero backoff; a huge backoff would make
	// a wrong path hang instead of fail.
	agents.Confucius.Retry = config.AgentRetryConfig{
		Enabled:        true,
		MaxAttempts:    3,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	}
	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{APIKey: "test", Models: testCatalog()},
		JWT:    config.JWTConfig{Secret: integrationTestSecret, Expire: "1h"},
		Agents: agents,
	}
	env := newTestEnvWithConfig(t, llm, cfg)

	token := authToken(t, defaultUserID)
	w := env.postMessage(`{"content":"hello"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	eventTypes, payloads := parseSSEBody(t, w.Body.String())

	// The turn completed: agent lifecycle + done, and the done event carries
	// no error (the retry saved the turn).
	requireEventSubsequence(t, eventTypes, []string{
		stream.EventAgentStart, stream.EventAgentError, stream.EventToken, stream.EventAgentEnd, stream.EventDone,
	})
	var donePayload map[string]any
	for i, typ := range eventTypes {
		if typ == stream.EventDone {
			donePayload = payloads[i]
		}
	}
	require.NotNil(t, donePayload, "done event missing")
	assert.Empty(t, donePayload["error"], "the retried turn must not surface an error")

	// The retry marker: agent_error with error_code=retry and meta.retry=true.
	var retryMarkers []map[string]any
	for i, typ := range eventTypes {
		if typ == stream.EventAgentError {
			meta, _ := payloads[i]["meta"].(map[string]any)
			if meta["retry"] == true {
				retryMarkers = append(retryMarkers, payloads[i])
			}
		}
	}
	require.Len(t, retryMarkers, 1, "exactly one retry marker expected, got %d", len(retryMarkers))
	assert.Contains(t, retryMarkers[0]["content"], "503")
	assert.Equal(t, stream.AgentConfucius, retryMarkers[0]["agent"])

	// Exactly two turn LLM calls (the failed attempt + the retry), and the
	// retry re-issued the identical request.
	turnCalls := llm.turnRequests()
	require.Len(t, turnCalls, 2)
	assert.Equal(t, turnCalls[0], turnCalls[1], "retry must re-send the byte-identical request")

	// The retry marker is persisted as an ordinary agent_error row. Messages
	// ride the write-behind queue, so wait for the dual-write then drain the
	// queue into the MySQL tier (the harness's stand-in for the flusher).
	env.waitForPersistedTurn(t, defaultSessionID, 5) // user + start/error/token/end
	var errRows int
	for _, m := range env.mysqlFake.messagesFor(defaultSessionID) {
		if m.EventType == model.EventTypeAgentError && m.Agent == stream.AgentConfucius {
			errRows++
			assert.Contains(t, m.Content, "503", "the persisted retry row carries the triggering error")
		}
	}
	assert.Equal(t, 1, errRows, "exactly one retry agent_error row")

	// turn_usage reflects BOTH attempts (real spend): 12 + 12 = 24.
	require.Eventually(t, func() bool {
		return len(env.mysqlFake.turnUsagesFor(defaultSessionID)) == 1
	}, 2*time.Second, 10*time.Millisecond, "expected one turn_usage row")
	row := env.mysqlFake.turnUsagesFor(defaultSessionID)[0]
	assert.Equal(t, 24, row.TotalTokens, "turn_usage must fold the failed attempt's reported usage")
}
