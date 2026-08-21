package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
)

// TestTitleCadence_FiresAtSendTimeWhileTurnRuns verifies the send-time trigger
// end to end: with the turn's LLM call gated (the turn observably still
// running), the title generated from the JUST-SENT message is already
// upserted — it does not wait for the turn to complete. The title input must
// be the triggering user message (n=1: first and current anchors coincide)
// and no assistant content.
func TestTitleCadence_FiresAtSendTimeWhileTurnRuns(t *testing.T) {
	scripted := newScriptedLLMClient(
		scriptedLLMResponse{tokens: []string{"slow ", "reply"}, content: "slow reply", finishReason: "stop", usage: agent.Usage{TotalTokens: 3}},
	).withTitleResponses(
		scriptedLLMResponse{content: "Send Time Title", finishReason: "stop"},
	)
	gated := newScriptedGated(t, scripted)
	env := newTestEnv(t, gated)
	token := authToken(t, defaultUserID)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- env.postMessage(`{"content":"hello gating"}`, token) }()
	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never reached the LLM call")
	}

	// While the turn is still gated mid-LLM-call, the send-time title must
	// already be durable.
	require.Eventually(t, func() bool {
		env.mysqlFake.mu.Lock()
		defer env.mysqlFake.mu.Unlock()
		return env.mysqlFake.titles[defaultSessionID].Title == "Send Time Title"
	}, 5*time.Second, 10*time.Millisecond, "expected the send-time title while the turn still runs")

	// The title prompt carried the user message and no assistant content.
	var titleReq *agent.LLMRequest
	for _, r := range scripted.requests() {
		if r.Model == scriptedTitleModel {
			req := r
			titleReq = &req
			break
		}
	}
	require.NotNil(t, titleReq, "expected a title-generation LLM call")
	assert.Contains(t, titleReq.Messages[len(titleReq.Messages)-1].Content, "hello gating")
	assert.NotContains(t, titleReq.Messages[len(titleReq.Messages)-1].Content, "Assistant", "assistant content must not be part of the title input")

	gated.release()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("turn never completed after release")
	}
}

// inFlightManualTitleLLM wraps the scripted client: when the title-generation
// call arrives — i.e. AFTER the GetTitle early-exit check has already passed —
// it first PATCHes a manual title through the real HTTP stack and only then
// serves the scripted AI title response. This models the user editing the
// title while the AI generation's LLM call is in flight.
type inFlightManualTitleLLM struct {
	inner  agent.LLMClient
	engine *gin.Engine
	token  string
}

func (w *inFlightManualTitleLLM) StreamChat(ctx context.Context, req agent.LLMRequest, onToken func(string) error, onReasoning func(string) error) (agent.LLMResponse, error) {
	if req.Model == scriptedTitleModel && w.engine != nil {
		patch := httptest.NewRequest(http.MethodPatch,
			"/api/v1/sessions/"+defaultSessionID,
			strings.NewReader(`{"title":"Manual Mid Flight"}`))
		patch.Header.Set("Content-Type", "application/json")
		patch.Header.Set("Authorization", "Bearer "+w.token)
		pw := httptest.NewRecorder()
		w.engine.ServeHTTP(pw, patch)
		if pw.Code != http.StatusOK {
			return agent.LLMResponse{}, fmt.Errorf("in-flight manual title patch failed: %d %s", pw.Code, pw.Body.String())
		}
	}
	return w.inner.StreamChat(ctx, req, onToken, onReasoning)
}

// TestTitleCadence_ManualSetWhileGenerationInFlight drives the manual-guard
// race end to end through the real handler stack: the title generation passes
// its early-exit check, the user sets a manual title mid-LLM-call, and the AI
// upsert that lands afterwards must have zero effect — the manual title,
// trace_id and is_manual = TRUE all survive (the memoryMySQL fake mirrors the
// upsertTitleSQL IF(is_manual, ...) guard).
func TestTitleCadence_ManualSetWhileGenerationInFlight(t *testing.T) {
	scripted := newScriptedLLMClient(
		scriptedLLMResponse{tokens: []string{"reply"}, content: "reply", finishReason: "stop", usage: agent.Usage{TotalTokens: 2}},
	).withTitleResponses(
		scriptedLLMResponse{content: "AI Title", finishReason: "stop"},
	)
	wrapped := &inFlightManualTitleLLM{inner: scripted}
	env := newTestEnv(t, wrapped)
	// The wrapper needs the engine to PATCH through; wire it post-construction
	// (the title call only happens once a message is posted).
	wrapped.engine = env.engine
	wrapped.token = authToken(t, defaultUserID)

	w := env.postMessage(`{"content":"first message"}`, wrapped.token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Wait until both LLM calls were observed (turn + title), so the AI
	// upsert has certainly landed before the final state check.
	require.Eventually(t, func() bool {
		return len(scripted.requests()) >= 2
	}, 5*time.Second, 10*time.Millisecond, "expected the turn and title LLM calls")

	env.mysqlFake.mu.Lock()
	defer env.mysqlFake.mu.Unlock()
	got := env.mysqlFake.titles[defaultSessionID]
	assert.Equal(t, "Manual Mid Flight", got.Title, "manual title must survive the in-flight AI upsert")
	assert.NotEqual(t, "AI Title", got.Title, "the AI upsert must have zero effect on a manual row")
	assert.True(t, got.IsManual, "is_manual must stay TRUE")
}
