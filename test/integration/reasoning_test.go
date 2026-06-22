package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

// TestMessageFlow_ReasoningConfig_Propagated verifies that when Confuse is
// configured with thinking=true and reasoning_effort=high, the orchestrator
// forwards those fields on the LLMRequest, streams reasoning events, persists
// reasoning content, and echoes it back in multi-turn context.
func TestMessageFlow_ReasoningConfig_Propagated(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:           []string{"Hello"},
			reasoningTokens:  []string{"Analyzing", " the greeting"},
			reasoningContent: "Analyzing the greeting",
			content:          "Hello",
			finishReason:     "stop",
			usage:            agent.Usage{PromptTokens: 10, CompletionTokens: 1, ReasoningTokens: 2, TotalTokens: 13},
		},
		scriptedLLMResponse{
			content:      "Greeting",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
		scriptedLLMResponse{
			content:      "Again",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	)
	env := newTestEnvWithAgentsConfig(t, llm, agentConfigWithReasoning())

	token := authToken(t, defaultUserID)
	w := env.postMessage(`{"content":"hello"}`, token)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	types, payloads := parseSSEBody(t, w.Body.String())
	requireEventSubsequence(t, types, []string{
		stream.EventAgentStart,
		stream.EventReasoning,
		stream.EventToken,
		stream.EventAgentEnd,
		stream.EventDone,
	})

	// Verify the done event includes reasoning_tokens (nested under meta.usage).
	var donePayload map[string]any
	for i, typ := range types {
		if typ == stream.EventDone {
			donePayload = payloads[i]
			break
		}
	}
	require.NotNil(t, donePayload, "expected done event")
	meta, ok := donePayload["meta"].(map[string]any)
	require.True(t, ok, "expected meta in done event")
	usage, ok := meta["usage"].(map[string]any)
	require.True(t, ok, "expected usage in done event meta")
	assert.Equal(t, float64(2), usage["reasoning_tokens"], "expected reasoning_tokens in usage")

	// Wait for the async batch save and title generation to finish so the test
	// can clean up the temp data directory without racing the FS writer.
	require.Eventually(t, func() bool {
		return len(env.mysqlFake.messagesFor(defaultSessionID)) >= 4
	}, 2*time.Second, 10*time.Millisecond, "expected first turn messages to be persisted")

	// Wait for the async title generation round to complete so the request
	// snapshot is stable.
	require.Eventually(t, func() bool {
		env.mysqlFake.mu.Lock()
		defer env.mysqlFake.mu.Unlock()
		return len(env.mysqlFake.titles[defaultSessionID].Title) > 0
	}, 2*time.Second, 10*time.Millisecond, "expected title to be generated")

	var reasoningReq *agent.LLMRequest
	for _, req := range llm.requests() {
		if req.Thinking {
			reasoningReq = &req
			break
		}
	}
	require.NotNil(t, reasoningReq, "expected a reasoning LLMRequest")
	assert.True(t, reasoningReq.Thinking, "Thinking must be true")
	assert.Equal(t, "high", reasoningReq.ReasoningEffort, "reasoning_effort must match config")
	assert.Equal(t, 512, reasoningReq.MaxTokens, "max_tokens must match config")

	// Verify reasoning content was persisted.
	var foundReasoning bool
	for _, m := range env.mysqlFake.messagesFor(defaultSessionID) {
		if m.EventType == "reasoning" {
			foundReasoning = true
			assert.Equal(t, "Analyzing the greeting", m.Content)
			assert.Equal(t, model.RoleAssistant, m.Role)
		}
	}
	assert.True(t, foundReasoning, "expected a persisted reasoning event")

	// Send a second turn and verify the prior reasoning content is echoed back.
	w2 := env.postMessage(`{"content":"again"}`, token)
	require.Equal(t, http.StatusOK, w2.Code, "body: %s", w2.Body.String())

	require.Eventually(t, func() bool {
		return len(env.mysqlFake.messagesFor(defaultSessionID)) >= 8
	}, 2*time.Second, 10*time.Millisecond, "expected second turn messages to be persisted")

	var echoed bool
	for _, req := range llm.requests() {
		for _, m := range req.Messages {
			if m.Role == "assistant" && m.ReasoningContent == "Analyzing the greeting" {
				echoed = true
			}
		}
	}
	assert.True(t, echoed, "expected prior reasoning content to be echoed back in second turn messages")
}
