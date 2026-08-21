package integration

import (
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

// TestMessageFlow_ReasoningConfig_Propagated verifies that with a
// thinking:true catalog entry and openai.default_reasoning_effort: high
// (model-effort-v2's deployment-level effort source), the orchestrator
// forwards the wire family + effort on the LLMRequest, streams reasoning
// events, persists reasoning content, and echoes it back in multi-turn
// context.
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
			content:      "Again",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	).withTitleResponses(
		scriptedLLMResponse{
			content:      "Greeting",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	)
	env := newTestEnvWithConfig(t, llm, &config.Config{
		OpenAI: config.OpenAIConfig{
			APIKey:                 "test",
			Models:                 []config.ModelCatalogEntry{{Name: "gpt-test", MaxContextTokens: 128000, MaxCompletionTokens: 512, Thinking: true}},
			DefaultReasoningEffort: "high",
		},
		JWT:    config.JWTConfig{Secret: integrationTestSecret, Expire: "1h"},
		Agents: agentConfig(),
	})

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
	// New authoritative shape: reasoning_tokens lives under total (and each
	// by_agent entry), not at the flat top level.
	totalObj, ok := usage["total"].(map[string]any)
	require.True(t, ok, "expected usage.total in done event")
	assert.Equal(t, float64(2), totalObj["reasoning_tokens"], "expected reasoning_tokens in usage.total")

	// Wait for the dual write, then drain the write-behind queue into the
	// fake MySQL tier.
	env.waitForPersistedTurn(t, defaultSessionID, 4)

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
	assert.Equal(t, "high", reasoningReq.ReasoningEffort, "reasoning_effort must match the deployment default")
	assert.Equal(t, 512, reasoningReq.MaxCompletionTokens, "quota must match the turn-resolved catalog entry")

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

	env.waitForPersistedTurn(t, defaultSessionID, 8)

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
