package integration

// End-to-end session-message-queue coverage over the real handler stack
// (miniredis + memoryMySQL + a gated scripted LLM): queue admission behind a
// running turn, automatic continuation after the turn's terminal persistence,
// per-message model selection, cancel of a queued message, and the depth cap.
// The queue-DISABLED regression (409 SESSION_BUSY) is covered by the existing
// turn_run tests, which run the default (queue off) harness unchanged.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/run"
)

// queueCatalogConfig is a two-model catalog so each queued message can carry
// a distinct model (per-request-model-selection independence end to end).
func queueCatalogConfig() *config.Config {
	return &config.Config{
		OpenAI: config.OpenAIConfig{
			APIKey: "test",
			Models: []config.ModelCatalogEntry{
				{Name: "gpt-5", MaxContextTokens: 400000, Thinking: true, MaxCompletionTokens: 512},
				{Name: "glm-4.7", MaxContextTokens: 200000, Thinking: false, MaxCompletionTokens: 512},
			},
			DefaultModel: "gpt-5",
		},
		JWT:    config.JWTConfig{Secret: integrationTestSecret, Expire: "1h"},
		Agents: agentConfig(),
	}
}

// newQueueEnv builds the full harness with the session message queue enabled
// (messages.queue.enabled equivalent): admission queues behind running turns
// and the turn goroutine pumps the head after Finalize.
func newQueueEnv(t *testing.T, llm agent.LLMClient, cfg *config.Config, maxDepth int) *testEnv {
	t.Helper()
	env := newTestEnvWithConfig(t, llm, cfg)
	env.streamH.EnableMessageQueue(maxDepth)
	env.sessH.EnableQueueInfo()
	return env
}

// postMessageAsync runs postMessage on its own goroutine — the queued POST's
// SSE response stays open (tailing) until its turn finishes.
func (e *testEnv) postMessageAsync(body, token string) chan *httptest.ResponseRecorder {
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() { out <- e.postMessage(body, token) }()
	return out
}

// getQueue reads the queue endpoint.
func (e *testEnv) getQueue(token string) (int, []map[string]any) {
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/sessions/"+defaultSessionID+"/queue", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		return w.Code, nil
	}
	var body struct {
		Queue []map[string]any `json:"queue"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		return -1, nil
	}
	return w.Code, body.Queue
}

// getSessionDetail reads the single-session detail endpoint.
func (e *testEnv) getSessionDetail(token string) map[string]any {
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/sessions/"+defaultSessionID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	require.Equal(e.t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var body map[string]any
	require.NoError(e.t, json.Unmarshal(w.Body.Bytes(), &body))
	return body
}

// TestQueue_FullChain covers the capability's spine end to end: message B is
// admitted into the FIFO queue while gated turn A streams, its POST response
// opens with the `queued` event and waits, A's terminal persistence + claim
// release pumps B as the next turn on the SAME run id, B runs to completion
// on its own model selection, and the persisted history orders
// userA < assistantA < userB < assistantB.
func TestQueue_FullChain(t *testing.T) {
	scripted := newScriptedLLMClient(
		scriptedLLMResponse{tokens: []string{"answer A"}, finishReason: "stop"},
		scriptedLLMResponse{tokens: []string{"answer B"}, finishReason: "stop"},
	)
	gated := newGatedLLM(scripted)
	env := newQueueEnv(t, gated, queueCatalogConfig(), 8)
	token := authToken(t, "")

	// Turn A holds the LLM gate (gated on first non-title call).
	wA := env.postMessageAsync(`{"content":"question A","model":"gpt-5"}`, token)
	<-gated.entered

	// Message B arrives while A runs: admitted into the queue, SSE tails.
	wB := env.postMessageAsync(`{"content":"question B","model":"glm-4.7"}`, token)
	require.Eventually(t, func() bool {
		code, entries := env.getQueue(token)
		return code == http.StatusOK && len(entries) == 1
	}, 2*time.Second, 20*time.Millisecond, "message B never appeared in the queue")

	code, entries := env.getQueue(token)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "question B", entries[0]["content"])
	assert.Equal(t, "glm-4.7", entries[0]["model"])
	queuedRunID, _ := entries[0]["run_id"].(string)
	require.NotEmpty(t, queuedRunID, "queue entry carries no run id")

	// Session detail reports the additive queued_count while the default
	// (queue-off) suites never see the field.
	detail := env.getSessionDetail(token)
	assert.EqualValues(t, 1, detail["queued_count"])
	assert.EqualValues(t, true, detail["generating"])

	// The queued run's meta is `queued` and its log starts with the event.
	store := env.redisSvc.RunStore()
	meta, ok, err := store.GetMeta(context.Background(), queuedRunID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, run.StatusQueued, meta.Status)

	// Release turn A: it finalizes, the pump continues B, both POSTs complete.
	gated.release()
	select {
	case recA := <-wA:
		require.Equal(t, http.StatusOK, recA.Code, "body: %s", recA.Body.String())
		assert.NotContains(t, recA.Body.String(), "event: queued", "turn A must not see a queued event")
	case <-time.After(5 * time.Second):
		t.Fatal("turn A never completed")
	}
	select {
	case recB := <-wB:
		require.Equal(t, http.StatusOK, recB.Code, "body: %s", recB.Body.String())
		body := recB.Body.String()
		assert.Equal(t, 1, strings.Count(body, "event: queued"))
		assert.Equal(t, 1, strings.Count(body, "event: done"))
		assert.Less(t, strings.Index(body, "event: queued"), strings.Index(body, "event: agent_start"),
			"queued event must precede the continued turn's events")
		assert.Equal(t, queuedRunID, recB.Header().Get("X-Run-Id"),
			"the continued turn must reuse the queued run's id")
	case <-time.After(5 * time.Second):
		t.Fatal("queued turn B never completed")
	}

	// The queue drained; the continued run finalized as done on B's model.
	require.Eventually(t, func() bool {
		_, entries := env.getQueue(token)
		return len(entries) == 0
	}, 2*time.Second, 20*time.Millisecond)
	meta, _, err = store.GetMeta(context.Background(), queuedRunID)
	require.NoError(t, err)
	assert.Equal(t, run.StatusDone, meta.Status)
	assert.Equal(t, "glm-4.7", meta.Model, "each queued message runs its own model selection")

	// Persisted history ordering: userA < assistantA < userB < assistantB
	// (the queued user row takes pump time — design D4).
	rows, err := env.mysqlFake.ListMessages(context.Background(), defaultSessionID)
	require.NoError(t, err)
	var roles []string
	var contents []string
	for _, r := range rows {
		if r.Role == model.RoleUser || r.Role == model.RoleAssistant {
			roles = append(roles, string(r.Role))
			contents = append(contents, r.Content)
		}
	}
	assert.Equal(t, []string{"user", "assistant", "user", "assistant"}, roles,
		"history rows: %v / %v", roles, contents)
	require.Len(t, contents, 4)
	assert.Contains(t, contents[0], "question A")
	assert.Contains(t, contents[1], "answer A")
	assert.Contains(t, contents[2], "question B")
	assert.Contains(t, contents[3], "answer B")
	// Strict monotonic msg_time across the four anchor rows.
	for i := 1; i < 4; i++ {
		assert.False(t, rows[i].MsgTime.Before(rows[i-1].MsgTime),
			"row %d (%s) predates row %d", i, contents[i], i-1)
	}
}

// TestQueue_CancelQueuedThenQueueContinues covers the queued-run cancel: the
// entry dequeues and finalizes as cancelled (never interrupted), and the
// running turn's later completion leaves the queue EMPTY — no phantom turn.
func TestQueue_CancelQueuedThenQueueContinues(t *testing.T) {
	scripted := newScriptedLLMClient(
		scriptedLLMResponse{tokens: []string{"answer A"}, finishReason: "stop"},
		// A second response would only be consumed if the cancelled message
		// were (wrongly) continued — its absence makes that a hard failure.
	)
	gated := newGatedLLM(scripted)
	env := newQueueEnv(t, gated, queueCatalogConfig(), 8)
	token := authToken(t, "")

	wA := env.postMessageAsync(`{"content":"question A","model":"gpt-5"}`, token)
	<-gated.entered
	wB := env.postMessageAsync(`{"content":"question B","model":"gpt-5"}`, token)
	require.Eventually(t, func() bool {
		_, entries := env.getQueue(token)
		return len(entries) == 1
	}, 2*time.Second, 20*time.Millisecond, "message B never enqueued")

	_, entries := env.getQueue(token)
	queuedRunID, _ := entries[0]["run_id"].(string)

	// Cancel the QUEUED message (it has no heartbeat by design).
	wCancel := env.turnRequest(http.MethodPost, queuedRunID, token)
	require.Equal(t, http.StatusOK, wCancel.Code, "body: %s", wCancel.Body.String())
	assert.Contains(t, wCancel.Body.String(), `"status":"cancelled"`)

	// Dequeued: the queue is empty, the run is terminal-cancelled.
	_, entries = env.getQueue(token)
	assert.Empty(t, entries)
	store := env.redisSvc.RunStore()
	meta, _, err := store.GetMeta(context.Background(), queuedRunID)
	require.NoError(t, err)
	assert.Equal(t, run.StatusCancelled, meta.Status)

	// Release turn A: it completes and the pump finds nothing to continue.
	gated.release()
	select {
	case <-wA:
	case <-time.After(5 * time.Second):
		t.Fatal("turn A never completed")
	}
	// wB's SSE response closed on the cancel (terminal status close path).
	select {
	case recB := <-wB:
		assert.Contains(t, recB.Body.String(), "event: queued",
			"the cancelled message's stream should still show its admission event")
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled queued stream never closed")
	}

	// No phantom turn: exactly one user row and one assistant row.
	rows, err := env.mysqlFake.ListMessages(context.Background(), defaultSessionID)
	require.NoError(t, err)
	var userRows, assistantRows int
	for _, r := range rows {
		switch r.Role {
		case model.RoleUser:
			userRows++
		case model.RoleAssistant:
			assistantRows++
		}
	}
	assert.Equal(t, 1, userRows, "the cancelled queued message must not persist a user row")
	assert.Equal(t, 1, assistantRows)
}

// TestQueue_DepthCapIntegration: admission beyond max_depth is a 409
// QUEUE_FULL and leaves the queue untouched.
func TestQueue_DepthCapIntegration(t *testing.T) {
	scripted := newScriptedLLMClient(
		scriptedLLMResponse{tokens: []string{"answer A"}, finishReason: "stop"},
		scriptedLLMResponse{tokens: []string{"answer B"}, finishReason: "stop"},
	)
	gated := newGatedLLM(scripted)
	env := newQueueEnv(t, gated, queueCatalogConfig(), 1)
	token := authToken(t, "")

	wA := env.postMessageAsync(`{"content":"question A","model":"gpt-5"}`, token)
	<-gated.entered
	wB := env.postMessageAsync(`{"content":"question B","model":"gpt-5"}`, token)
	require.Eventually(t, func() bool {
		_, entries := env.getQueue(token)
		return len(entries) == 1
	}, 2*time.Second, 20*time.Millisecond)

	// The queue is full (cap 1): message C is rejected.
	wC := env.postMessage(`{"content":"question C","model":"gpt-5"}`, token)
	require.Equal(t, http.StatusConflict, wC.Code, "body: %s", wC.Body.String())
	assert.Contains(t, wC.Body.String(), "QUEUE_FULL")
	_, entries := env.getQueue(token)
	assert.Len(t, entries, 1, "QUEUE_FULL must not change the queue")

	gated.release()
	select {
	case <-wA:
	case <-time.After(5 * time.Second):
		t.Fatal("turn A never completed")
	}
	// The queued message B continues after A and must settle too (goleak).
	select {
	case <-wB:
	case <-time.After(5 * time.Second):
		t.Fatal("queued turn B never completed")
	}
}

// TestQueue_DisabledKeeps409 pins the default-off contract at the integration
// level: without EnableMessageQueue the busy session still answers 409
// SESSION_BUSY carrying the active run id, and no queue state appears.
func TestQueue_DisabledKeeps409(t *testing.T) {
	scripted := newScriptedLLMClient(
		scriptedLLMResponse{tokens: []string{"answer A"}, finishReason: "stop"},
	)
	gated := newGatedLLM(scripted)
	env := newTestEnvWithConfig(t, gated, queueCatalogConfig()) // queue OFF
	token := authToken(t, "")

	wA := env.postMessageAsync(`{"content":"question A","model":"gpt-5"}`, token)
	<-gated.entered

	// The competing message is rejected (the pre-capability behavior).
	wB := env.postMessage(`{"content":"question B","model":"gpt-5"}`, token)
	require.Equal(t, http.StatusConflict, wB.Code, "body: %s", wB.Body.String())
	var body struct {
		Error struct {
			Code  string `json:"code"`
			RunID string `json:"run_id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(wB.Body.Bytes(), &body))
	assert.Equal(t, "SESSION_BUSY", body.Error.Code)
	assert.NotEmpty(t, body.Error.RunID)

	// The queue endpoint exists (it is a static route) but stays empty.
	code, entries := env.getQueue(token)
	assert.Equal(t, http.StatusOK, code)
	assert.Empty(t, entries)
	// Session detail carries no queued_count with the queue disabled.
	detail := env.getSessionDetail(token)
	_, present := detail["queued_count"]
	assert.False(t, present, "disabled handler must not emit queued_count")

	gated.release()
	select {
	case <-wA:
	case <-time.After(5 * time.Second):
		t.Fatal("turn A never completed")
	}
}
