package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/stream"
)

// TestMessageFlow_ReasoningConfig_Propagated verifies that when Confuse is
// configured with thinking=true and reasoning_effort=high, the orchestrator
// forwards those fields on the LLMRequest it sends to the underlying client.
func TestMessageFlow_ReasoningConfig_Propagated(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"Hello"},
			content:      "Hello",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
		},
		scriptedLLMResponse{
			content:      "Greeting",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	)
	env := newTestEnvWithAgentsConfig(t, llm, agentConfigWithReasoning())

	token := authToken(t, defaultUserID)
	w := env.postMessage(`{"content":"hello"}`, token)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	types, _ := parseSSEBody(t, w.Body.String())
	requireEventSubsequence(t, types, []string{
		stream.EventAgentStart,
		stream.EventToken,
		stream.EventAgentEnd,
		stream.EventDone,
	})

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
}
