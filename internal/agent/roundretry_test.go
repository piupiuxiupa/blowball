package agent

import (
	"context"
	"testing"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/xizhi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- llm-round-retry unit tests (tasks 3.1-3.7) -----------------------------
//
// The retry-under-test wraps the single StreamChat call inside runLLMRound
// (streamChatWithRetry). These tests exercise it through runLLMRound — the
// production entry — so the folding/event/error contracts are asserted on the
// same roundResult the three agent loops consume.

// fastRetryPolicy returns an enabled policy with a near-zero backoff so tests
// exercise the retry path without sleeping. A huge-backoff variant
// (noRetryBackoffPolicy) is used where a wrong retry decision would hang.
func fastRetryPolicy(maxAttempts int) config.AgentRetryConfig {
	return config.AgentRetryConfig{
		Enabled:        true,
		MaxAttempts:    maxAttempts,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	}
}

// hugeRetryPolicy is enabled with an hour-scale backoff: any code path that
// wrongly enters the backoff sleep visibly hangs the test instead of passing.
func hugeRetryPolicy(maxAttempts int) config.AgentRetryConfig {
	return config.AgentRetryConfig{
		Enabled:        true,
		MaxAttempts:    maxAttempts,
		InitialBackoff: time.Hour,
		MaxBackoff:     time.Hour,
	}
}

// retryMarkerEvents filters the retry markers: agent_error events whose Meta
// carries retry=true.
func retryMarkerEvents(events []stream.StreamEvent) []stream.StreamEvent {
	var out []stream.StreamEvent
	for _, e := range events {
		if e.Type == stream.EventAgentError && e.Meta["retry"] == true {
			out = append(out, e)
		}
	}
	return out
}

// runRound is the shared harness: one runLLMRound call against the fake client
// under collectEvents, with the plain rebuild/onToken closures every test uses.
// round is a pointer so scaffold appends are visible to the caller.
func runRound(t *testing.T, ctx context.Context, client *fakeLLMClient, agentName string,
	round *[]Message, req LLMRequest, lc config.LengthContinueConfig, retry config.AgentRetryConfig) (roundResult, error, []stream.StreamEvent) {
	t.Helper()
	var res roundResult
	var err error
	events := collectEvents(t, func(hub *stream.Hub) {
		res, err = runLLMRound(ctx, client, hub, agentName, req, round, lc, retry,
			func(r []Message) []Message { return withSystem("sys", r) }, nil,
			func(string) error { return nil }, func(string) error { return nil })
	})
	return res, err, events
}

// 3.1: a mid-stream transient failure (watchdog-timeout text) is retried with
// a byte-identical request; the round completes; exactly one retry marker;
// usage sums both attempts; content keeps only the successful attempt.
func TestRunLLMRound_TransientRetrySucceeds(t *testing.T) {
	client := newFake(
		fakeResponse{err: errString("llm stream idle timeout: no frames for 2m0s"), usage: Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}},
		fakeResponse{content: "answer", finishReason: "stop", usage: Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
	)
	round := []Message{{Role: "user", Content: "q"}}
	req := LLMRequest{Model: "m", Messages: withSystem("sys", round), MaxCompletionTokens: 1000}

	res, err, events := runRound(t, context.Background(), client, "Liang", &round, req, config.LengthContinueConfig{}, fastRetryPolicy(3))
	require.NoError(t, err)

	// The retry re-issued the byte-identical request.
	require.Equal(t, 2, client.requestCount())
	assert.Equal(t, client.calls[0], client.calls[1], "retry must re-send the byte-identical request")

	// Exactly one retry marker: agent_error, code "retry", Meta.retry=true,
	// carrying the triggering error.
	markers := retryMarkerEvents(events)
	require.Len(t, markers, 1)
	assert.Equal(t, "retry", markers[0].Meta[stream.MetaCode])
	assert.Equal(t, "Liang", markers[0].Agent)
	assert.Contains(t, markers[0].Content, "timeout")

	// Real-spend accounting: both attempts' usage; content is only the
	// successful attempt's (the failed attempt emitted no content here).
	assert.Equal(t, 27, res.Usage.TotalTokens)
	assert.Equal(t, "answer", res.Content)
	assert.Equal(t, "stop", res.Resp.FinishReason)
}

// 3.2: a failure under a cancelled parent context is returned immediately —
// zero retry markers, zero extra calls, and (guarded by the hour-scale
// backoff) zero backoff sleeps.
func TestRunLLMRound_CancelNeverRetried(t *testing.T) {
	client := newFake(
		fakeResponse{err: context.Canceled},
		// A second queued response proves it is never consumed.
		fakeResponse{content: "should not run", finishReason: "stop"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	round := []Message{{Role: "user", Content: "q"}}
	req := LLMRequest{Model: "m", Messages: round, MaxCompletionTokens: 100}

	res, err, events := runRound(t, ctx, client, "Liang", &round, req, config.LengthContinueConfig{}, hugeRetryPolicy(3))
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, client.requestCount(), "cancelled context must not retry")
	assert.Empty(t, retryMarkerEvents(events))
	assert.Empty(t, res.Content)
}

// 3.3: non-transient errors fail fast — including the length-exhausted text,
// which deliberately avoids transient substrings so it never re-enters any
// retry pipeline.
func TestRunLLMRound_NonTransientNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"gateway rejection", errString("invalid request: unknown parameter foo")},
		{"length exhausted text", errString(lengthExhaustedMessage("Liang", 3, 32768))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newFake(
				fakeResponse{err: tc.err},
				fakeResponse{content: "should not run", finishReason: "stop"},
			)
			round := []Message{{Role: "user", Content: "q"}}
			req := LLMRequest{Model: "m", Messages: round, MaxCompletionTokens: 100}

			_, err, events := runRound(t, context.Background(), client, "Liang", &round, req, config.LengthContinueConfig{}, fastRetryPolicy(3))
			assert.ErrorIs(t, err, tc.err)
			assert.Equal(t, 1, client.requestCount(), "non-transient error must fail fast")
			assert.Empty(t, retryMarkerEvents(events))
		})
	}
}

// 3.4: consecutive transient failures surface the error after max_attempts
// total calls, having emitted max_attempts-1 retry markers.
func TestRunLLMRound_AttemptsExhausted(t *testing.T) {
	client := newFake(
		fakeResponse{err: errString("429 Too Many Requests"), usage: Usage{TotalTokens: 3}},
		fakeResponse{err: errString("503 Service Unavailable"), usage: Usage{TotalTokens: 3}},
		fakeResponse{err: errString("connection reset by peer"), usage: Usage{TotalTokens: 3}},
	)
	round := []Message{{Role: "user", Content: "q"}}
	req := LLMRequest{Model: "m", Messages: round, MaxCompletionTokens: 100}

	res, err, events := runRound(t, context.Background(), client, "Liang", &round, req, config.LengthContinueConfig{}, fastRetryPolicy(3))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection reset")
	assert.Equal(t, 3, client.requestCount(), "max_attempts counts the original call")
	assert.Len(t, retryMarkerEvents(events), 2, "retry markers = max_attempts-1")
	// Real spend: every failed attempt's reported usage is folded even when
	// the round ultimately fails.
	assert.Equal(t, 9, res.Usage.TotalTokens)
}

// 3.5: orthogonal to length continuation — a continuation attempt's transient
// failure is retried with the ALREADY-EXPANDED budget byte-identically, the
// continuation counter is not consumed, and the scaffolding is not duplicated.
func TestRunLLMRound_ContinuationAttemptRetried(t *testing.T) {
	client := newFake(
		fakeResponse{content: "part1", finishReason: "length"},
		fakeResponse{err: errString("unexpected EOF")},
		fakeResponse{content: "part2", finishReason: "stop"},
	)
	round := []Message{{Role: "user", Content: "q"}}
	req := LLMRequest{Model: "m", Messages: round, MaxCompletionTokens: 8192}

	res, err, events := runRound(t, context.Background(), client, "Liang", &round, req, enabledPolicy(4096, 3), fastRetryPolicy(3))
	require.NoError(t, err)

	// Attempt 0: base budget, finish_reason=length. Attempts 1 (failed) and 2
	// (retry) BOTH carry the expanded budget — the retry re-sends the
	// continuation request as-is.
	require.Equal(t, 3, client.requestCount())
	assert.Equal(t, 8192, client.calls[0].MaxCompletionTokens)
	assert.Equal(t, 8192+4096, client.calls[1].MaxCompletionTokens)
	assert.Equal(t, client.calls[1], client.calls[2], "retry must re-send the expanded request byte-identically")

	// Content accumulates across the continuation; one retry marker only.
	assert.Equal(t, "part1part2", res.Content)
	assert.Len(t, retryMarkerEvents(events), 1)

	// Scaffolding appended exactly once: user, assistant(partial),
	// user(instruction) — a duplicate scaffold would double the trailing pair.
	require.Len(t, round, 3)
	assert.Equal(t, "assistant", round[1].Role)
	assert.Equal(t, "part1", round[1].Content)
	assert.Equal(t, lengthContinueInstruction, round[2].Content)
}

// 3.6: the turn-level shared budget — a failed attempt's reported usage is
// charged before the retry decision, and an exhausted budget stops the retry;
// a child context (the dispatch shape) reads the same instance Confucius
// injected.
func TestRunLLMRound_BudgetStopsRetry(t *testing.T) {
	client := newFake(
		fakeResponse{err: errString("502 Bad Gateway"), usage: Usage{PromptTokens: 40, CompletionTokens: 10, TotalTokens: 50}},
		fakeResponse{content: "should not run", finishReason: "stop"},
	)
	budget := newRetryBudget(50) // exactly the failed attempt's spend
	ctx := WithRetryBudget(context.Background(), budget)

	round := []Message{{Role: "user", Content: "q"}}
	req := LLMRequest{Model: "m", Messages: round, MaxCompletionTokens: 100}

	_, err, events := runRound(t, ctx, client, "Liang", &round, req, config.LengthContinueConfig{}, fastRetryPolicy(3))
	require.Error(t, err)
	assert.Equal(t, 1, client.requestCount(), "exhausted budget must stop the retry")
	assert.Empty(t, retryMarkerEvents(events))

	// The dispatch shape: a context DERIVED from the injected one (as
	// dispatchToolCalls derives gctx) sees the same budget instance, so
	// sub-agent round retries charge the same pool.
	child, cancel := context.WithCancel(WithRetryBudget(context.Background(), budget))
	defer cancel()
	assert.Same(t, budget, retryBudgetFromCtx(child))
}

// 3.7: the round-cap wrap-up round inherits the owning agent's retry policy —
// a transient failure there is retried and the wrap-up still synthesizes.
func TestLiangRun_WrapUpRoundRetriesTransient(t *testing.T) {
	reg := tool.NewRegistry()
	xizhi.RegisterAll(reg, t.TempDir(), testXizhiConfigLike())
	client := newFake(
		// Round 1 (last allowed): dispatches a tool call -> cap -> wrap-up.
		fakeResponse{finishReason: "tool_calls", toolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunction{Name: xizhi.NameReadFile, Arguments: `{"path":"x.txt"}`}}}},
		// Wrap-up attempt 1: transient failure.
		fakeResponse{err: errString("504 Gateway Timeout")},
		// Wrap-up retry: the synthesized answer.
		fakeResponse{content: "final answer", finishReason: "stop"},
	)
	cfg := testLiangCfg()
	cfg.MaxRounds = 1
	cfg.Tools = []string{xizhi.NameReadFile}
	cfg.Retry = fastRetryPolicy(3)
	l, err := NewLiang(cfg, client, reg, ModelOverride{Model: "m", MaxCompletionTokens: 8192})
	require.NoError(t, err)

	var content string
	var runErr error
	events := collectEvents(t, func(hub *stream.Hub) {
		content, _, _, runErr = l.Run(context.Background(), []Message{{Role: "user", Content: "q"}}, hub)
	})
	require.NoError(t, runErr)
	assert.Equal(t, "final answer", content)

	// The wrap-up round's retry: same agent name, retry marker present, and
	// the retried request carries the wrap-up steering instruction (rebuild
	// applied at entry and unchanged by the retry).
	require.Equal(t, 3, client.requestCount())
	markers := retryMarkerEvents(events)
	require.Len(t, markers, 1)
	assert.Equal(t, "Liang", markers[0].Agent)
	wrapReq := client.calls[1]
	retriedReq := client.calls[2]
	assert.Equal(t, wrapReq, retriedReq, "wrap-up retry must re-send the identical request")
	lastMsg := retriedReq.Messages[len(retriedReq.Messages)-1]
	assert.Equal(t, wrapUpInstruction, lastMsg.Content, "the wrap-up steering instruction must survive the retry")
}
