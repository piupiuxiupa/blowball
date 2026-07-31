package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
)

// TestTurnUsage_PersistedOnSuccess verifies that a completed turn writes a
// turn_usage row (turn-cost-tracking spec): the row carries the session/trace/
// user identifiers, the authoritative usage JSON with the nested
// {total, by_agent, meta} shape, and a redundant total_tokens matching
// total.total_tokens.
func TestTurnUsage_PersistedOnSuccess(t *testing.T) {
	llm := newScriptedLLMClient(
		// Confucius direct answer.
		scriptedLLMResponse{
			tokens:       []string{"Hello"},
			content:      "Hello",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		},
		// TitleService round (async, first turn).
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

	// The turn_usage row is written in the persist goroutine (detached ctx),
	// so poll until it lands.
	require.Eventually(t, func() bool {
		return len(env.mysqlFake.turnUsagesFor(defaultSessionID)) == 1
	}, 2*time.Second, 10*time.Millisecond, "expected exactly one turn_usage row")

	rows := env.mysqlFake.turnUsagesFor(defaultSessionID)
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, defaultSessionID, row.SessionID)
	assert.Equal(t, defaultUserID, row.UserID)
	assert.NotEmpty(t, row.TraceID)
	assert.Equal(t, 12, row.TotalTokens, "redundant total_tokens must equal the turn total")

	// usage_json must carry the authoritative {total, by_agent, meta} shape.
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(row.UsageJSON), &doc))
	total, ok := doc["total"].(map[string]any)
	require.True(t, ok, "usage_json.total must be an object")
	assert.Equal(t, float64(12), total["total_tokens"])
	byAgent, ok := doc["by_agent"].(map[string]any)
	require.True(t, ok, "usage_json.by_agent must be an object")
	assert.Contains(t, byAgent, "Confucius", "by_agent must include Confucius for a direct answer")
	meta, ok := doc["meta"].(map[string]any)
	require.True(t, ok, "usage_json.meta must be an object")
	assert.Equal(t, false, meta["parallel"])
}

// TestTurnUsage_UsageWriteFailureDoesNotRollbackMessages verifies the
// turn-cost-tracking spec's "Usage write failure does not roll back messages"
// scenario: when SaveTurnUsage errors, the message batch still lands in MySQL
// and the SSE response / persistence path is unaffected.
func TestTurnUsage_UsageWriteFailureDoesNotRollbackMessages(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"Hello"},
			content:      "Hello",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		},
		scriptedLLMResponse{
			content:      "Greeting",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	)
	env := newTestEnv(t, llm)
	// Inject a usage-write failure: SaveTurnUsage returns an error, but the
	// message batch must still succeed (usage is observability data, messages
	// are business data).
	env.mysqlFake.mu.Lock()
	env.mysqlFake.saveTurnUsageErr = assertAnError("turn_usage unavailable")
	env.mysqlFake.mu.Unlock()

	token := authToken(t, defaultUserID)
	w := env.postMessage(`{"content":"hello"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Messages must still be persisted even though turn_usage failed.
	require.Eventually(t, func() bool {
		return len(env.mysqlFake.messagesFor(defaultSessionID)) >= 2
	}, 2*time.Second, 10*time.Millisecond, "message batch must persist despite usage-write failure")
	assert.Empty(t, env.mysqlFake.turnUsagesFor(defaultSessionID), "no turn_usage row on failure")
}

// TestTurnUsage_CascadesWithSessionDeletion verifies the FK ON DELETE CASCADE
// (migration 010): deleting a session removes its turn_usage rows. The in-memory
// fake mirrors the cascade so this stays accurate against the real schema.
func TestTurnUsage_CascadesWithSessionDeletion(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"Hello"},
			content:      "Hello",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		},
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

	require.Eventually(t, func() bool {
		return len(env.mysqlFake.turnUsagesFor(defaultSessionID)) == 1
	}, 2*time.Second, 10*time.Millisecond, "expected one turn_usage row before delete")

	// Delete the session; turn_usage rows must cascade away.
	delReq, err := http.NewRequest(http.MethodDelete, "/api/v1/sessions/"+defaultSessionID, nil)
	require.NoError(t, err)
	delReq.Header.Set("Authorization", "Bearer "+token)
	delW := httptest.NewRecorder()
	env.engine.ServeHTTP(delW, delReq)
	require.Equal(t, http.StatusNoContent, delW.Code)

	assert.Empty(t, env.mysqlFake.turnUsagesFor(defaultSessionID),
		"turn_usage rows must cascade with session deletion")
}

// assertAnError is a tiny helper returning a non-nil error with a message, used
// to inject simulated store failures in tests.
func assertAnError(msg string) error { return &simpleError{msg: msg} }

type simpleError struct{ msg string }

func (e *simpleError) Error() string { return e.msg }
