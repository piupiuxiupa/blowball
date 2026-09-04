package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// --- subagent-partial-output-on-failure unit tests (tasks 3.1-3.4) ----------
//
// Layer 1: a sub-agent Run's error return carries the failed round's already
// streamed assistant text (formerly the always-empty finalContent). Layer 2:
// dispatchSubAgent's give-up points join that partial with the error text —
// a blank partial keeps the legacy error-only content byte-identical.

// partialJoinMarker is the fixed separator joinFailure inserts between the
// error text and a non-blank partial output.
const partialJoinMarker = "\n\n--- partial output before failure ---\n\n"

// 3.1: joinFailure — blank partial returns the bare error text byte-for-byte;
// non-blank partial joins after the fixed marker line, inner shape verbatim.
func TestJoinFailure(t *testing.T) {
	err := errString("chongzhi: stream chat: unexpected EOF")

	t.Run("empty partial keeps error text byte-identical", func(t *testing.T) {
		assert.Equal(t, "chongzhi: stream chat: unexpected EOF", joinFailure(err, ""))
	})
	t.Run("whitespace-only partial keeps error text byte-identical", func(t *testing.T) {
		assert.Equal(t, "chongzhi: stream chat: unexpected EOF", joinFailure(err, "  \n\t "))
	})
	t.Run("non-empty partial joins after the marker", func(t *testing.T) {
		assert.Equal(t,
			"chongzhi: stream chat: unexpected EOF"+partialJoinMarker+"wrote x.txt",
			joinFailure(err, "wrote x.txt"))
	})
	t.Run("multiline partial keeps its inner shape verbatim", func(t *testing.T) {
		assert.Equal(t,
			"chongzhi: stream chat: unexpected EOF"+partialJoinMarker+"line1\nline2\n\nline3",
			joinFailure(err, "line1\nline2\n\nline3"))
	})
}

// 3.2: llm_error — the fake streams two tokens before the error surfaces, so
// Run's error return must carry the already-streamed assistantText. The
// agent's retry policy is left at the zero value (disabled) so the round
// fails on the first StreamChat call.
func TestChongzhi_Run_LLMErrorReturnsStreamedPartial(t *testing.T) {
	client := newFake(
		fakeResponse{tokens: []string{"wrote ", "x.txt"}, err: errString("unexpected EOF")},
	)
	chongzhi, err := newGenericFromAgentConfig(config.AgentConfig{
		Name:         "Chongzhi",
		SystemPrompt: "sys",
	}, client, tool.NewRegistry(), testTurn())
	require.NoError(t, err)

	var content string
	var runErr error
	collectEvents(t, func(hub *stream.Hub) {
		content, _, _, runErr = chongzhi.Run(context.Background(), []Message{{Role: "user", Content: "go"}}, hub)
	})
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "stream chat")
	assert.Equal(t, "wrote x.txt", content, "llm_error return must carry the failed round's streamed tokens")
}

// 3.2: symmetric coverage for Liang.
func TestLiang_Run_LLMErrorReturnsStreamedPartial(t *testing.T) {
	client := newFake(
		fakeResponse{tokens: []string{"half ", "analysis"}, err: errString("connection reset by peer")},
	)
	liang, err := newGenericFromAgentConfig(config.AgentConfig{
		Name:         "Liang",
		SystemPrompt: "sys",
	}, client, tool.NewRegistry(), testTurn())
	require.NoError(t, err)

	var content string
	var runErr error
	collectEvents(t, func(hub *stream.Hub) {
		content, _, _, runErr = liang.Run(context.Background(), []Message{{Role: "user", Content: "go"}}, hub)
	})
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "stream chat")
	assert.Equal(t, "half analysis", content, "llm_error return must carry the failed round's streamed tokens")
}

// dispatchOneSub drives dispatchSubAgent directly (same package) so the
// returned toolResult — content AND isError — is assertable without a full
// Confucius run. The give-up semantics under test live entirely in this
// method; the end-to-end feeding back into Confucius's next round is covered
// separately via runConfuciusRetry.
func dispatchOneSub(t *testing.T, sub Agent, invokeName string) toolResult {
	t.Helper()
	coordinator := testSpawnCoordinator(nil, func(SubAgentSpec) (Agent, error) { return sub, nil })
	tc := ToolCall{ID: "c1", Function: ToolCallFunction{Name: invokeName, Arguments: `{"task":"do"}`}}
	var result toolResult
	collectEvents(t, func(hub *stream.Hub) {
		result = coordinator.dispatch(context.Background(), tc, hub, newRetryBudget(0), spawnParent{agentName: "Confucius"})
	})
	return result
}

// msPolicy is an enabled retry policy with near-zero backoff for dispatch
// tests that exercise the retry loop without sleeping.
func msPolicy(maxAttempts int) config.AgentRetryConfig {
	return config.AgentRetryConfig{
		Enabled:        true,
		MaxAttempts:    maxAttempts,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
	}
}

// 3.3: a non-retried failure (disabled policy) with partial output — the
// give-up tool result joins error text + partial and keeps isError.
func TestDispatchSubAgent_GiveUpJoinsPartial(t *testing.T) {
	liang := &scriptedRetryAgent{
		name:     "Liang",
		policy:   config.AgentRetryConfig{Enabled: false, MaxAttempts: 3},
		outcomes: []scriptedOutcome{{err: transientErr, partial: "analysis so far", usage: Usage{TotalTokens: 5}}},
	}
	result := dispatchOneSub(t, liang, SpawnSubagentTool)
	assert.Equal(t, 1, liang.callCount())
	assert.True(t, result.isError)
	assert.True(t, strings.HasPrefix(result.content, transientErr.Error()+partialJoinMarker+"analysis so far"))
	assert.Contains(t, result.content, "status: error")
}

// 3.3 (zero regression): blank partial keeps the tool result content
// byte-level identical to the legacy error-only text.
func TestDispatchSubAgent_BlankPartialKeepsErrorText(t *testing.T) {
	liang := &scriptedRetryAgent{
		name:     "Liang",
		policy:   config.AgentRetryConfig{Enabled: false, MaxAttempts: 3},
		outcomes: []scriptedOutcome{{err: transientErr, usage: Usage{TotalTokens: 5}}},
	}
	result := dispatchOneSub(t, liang, SpawnSubagentTool)
	assert.Equal(t, 1, liang.callCount())
	assert.True(t, result.isError)
	assert.True(t, strings.HasPrefix(result.content, transientErr.Error()))
	assert.Contains(t, result.content, "status: error")
}

// 3.3 (end-to-end): the merged text is what Confucius actually consumes — the
// role="tool" message of its next round AND the SSE tool_result event carry
// the same joined content.
func TestConfucius_SubAgentFailure_PartialOutputFedBack(t *testing.T) {
	defer goleak.VerifyNone(t)
	liang := &scriptedRetryAgent{
		name:     "Liang",
		policy:   config.AgentRetryConfig{Enabled: false, MaxAttempts: 3},
		outcomes: []scriptedOutcome{{err: transientErr, partial: "half-done analysis", usage: Usage{TotalTokens: 5}}},
	}
	content, calls, events := runConfuciusRetry(t, liang, SpawnSubagentTool)
	require.Equal(t, 1, calls)

	want := transientErr.Error() + partialJoinMarker + "half-done analysis"
	assert.True(t, strings.HasPrefix(content, want), "the merged text must be fed back as the tool result")

	var sseContent string
	for _, e := range events {
		if e.Type == stream.EventToolResult && e.Agent == "Confucius" && e.Meta[stream.MetaToolCallID] == "c1" {
			sseContent = e.Content
		}
	}
	assert.True(t, strings.HasPrefix(sseContent, want), "the SSE tool_result event must carry the same merged text")
}

// 3.4: retry exhaustion carries the LAST attempt's partial output — every
// sub.Run overwrites content, so earlier attempts' partials are not kept
// (one-logical-call retry semantics).
func TestDispatchSubAgent_RetryExhaustedKeepsLastPartial(t *testing.T) {
	secondErr := errors.New("503 Service Unavailable")
	liang := &scriptedRetryAgent{
		name:   "Liang",
		policy: msPolicy(2),
		outcomes: []scriptedOutcome{
			{err: transientErr, partial: "first attempt partial", usage: Usage{TotalTokens: 5}},
			{err: secondErr, partial: "second attempt partial", usage: Usage{TotalTokens: 5}},
		},
	}
	result := dispatchOneSub(t, liang, SpawnSubagentTool)
	assert.Equal(t, 2, liang.callCount(), "max_attempts counts the original call")
	assert.True(t, result.isError)
	assert.True(t, strings.HasPrefix(result.content, secondErr.Error()+partialJoinMarker+"second attempt partial"),
		"the give-up point must join the LAST attempt's partial")
}

// 3.4: length_exhausted — the sub-agent's error return carries the partial
// accumulated across continuation attempts (result.Content on that path),
// which the give-up point joins after the error text. The length-exhausted
// message is deliberately non-transient, so the dispatch never retries it.
func TestDispatchSubAgent_LengthExhaustedJoinsAccumulated(t *testing.T) {
	exhausted := errString(lengthExhaustedMessage("Liang", 3, 32768))
	liang := &scriptedRetryAgent{
		name:     "Liang",
		policy:   msPolicy(3),
		outcomes: []scriptedOutcome{{err: exhausted, partial: "a very long answer that got cut", usage: Usage{TotalTokens: 9}}},
	}
	result := dispatchOneSub(t, liang, SpawnSubagentTool)
	assert.Equal(t, 1, liang.callCount(), "length-exhausted text is non-transient: never retried")
	assert.True(t, result.isError)
	assert.True(t, strings.HasPrefix(result.content, exhausted.Error()+partialJoinMarker+"a very long answer that got cut"))
}
