package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/xizhi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- harness ---------------------------------------------------------------

// collectEvents runs fn against a fresh hub with a concurrent consumer
// (mirroring runConfuciusAndCollect's drain-first pattern) and returns every
// event that was emitted.
func collectEvents(t *testing.T, fn func(hub *stream.Hub)) []stream.StreamEvent {
	t.Helper()
	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var mu sync.Mutex
	var events []stream.StreamEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case e := <-hub.Events():
				mu.Lock()
				events = append(events, e)
				mu.Unlock()
			default:
				select {
				case <-ctx.Done():
					return
				case <-hub.Done():
				drain:
					for {
						select {
						case e := <-hub.Events():
							mu.Lock()
							events = append(events, e)
							mu.Unlock()
						default:
							break drain
						}
					}
					return
				case e := <-hub.Events():
					mu.Lock()
					events = append(events, e)
					mu.Unlock()
				}
			}
		}
	}()
	fn(hub)
	hub.Close()
	<-done
	mu.Lock()
	defer mu.Unlock()
	return events
}

func enabledPolicy(step, retries int) config.LengthContinueConfig {
	return config.LengthContinueConfig{ExpandStep: step, MaxRetries: retries}
}

func findEvents(events []stream.StreamEvent, typ string) []stream.StreamEvent {
	var out []stream.StreamEvent
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// --- runLLMRound unit tests (design D2-D7) ----------------------------------

// One content-only length round continues with an expanded budget; content
// accumulates; the scaffold is assistant(partial) + user(instruction).
func TestRunLLMRound_ContentLengthContinues(t *testing.T) {
	client := newFake(
		fakeResponse{content: "alpha ", tokens: []string{"alpha "}, finishReason: "length", usage: Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
		fakeResponse{content: "beta", tokens: []string{"beta"}, finishReason: "stop", usage: Usage{PromptTokens: 11, CompletionTokens: 4, TotalTokens: 15}},
	)
	round := []Message{{Role: "user", Content: "write"}}
	req := LLMRequest{Model: "m", Messages: withSystem("sys", round), MaxCompletionTokens: 1000}

	var res roundResult
	var err error
	events := collectEvents(t, func(hub *stream.Hub) {
		res, err = runLLMRound(context.Background(), client, hub, "Liang", req, &round,
			enabledPolicy(512, 3), config.AgentRetryConfig{}, func(r []Message) []Message { return withSystem("sys", r) }, nil,
			func(string) error { return nil }, func(string) error { return nil })
	})
	require.NoError(t, err)

	// Two attempts; the second carries the expanded budget (design D6).
	require.Equal(t, 2, client.requestCount())
	assert.Equal(t, 1000, client.calls[0].MaxCompletionTokens)
	assert.Equal(t, 1512, client.calls[1].MaxCompletionTokens)

	// Content accumulates across attempts (design D2/D4); usage sums; the
	// final response is the stop attempt; LengthHit is false.
	assert.Equal(t, "alpha beta", res.Content)
	assert.Equal(t, 30, res.Usage.TotalTokens)
	assert.Equal(t, "stop", res.Resp.FinishReason)
	assert.False(t, res.LengthHit)

	// Scaffold: assistant(partial) then user(continuation instruction) — and
	// the continuation request's messages were rebuilt from the round.
	require.GreaterOrEqual(t, len(round), 3)
	assert.Equal(t, "assistant", round[1].Role)
	assert.Equal(t, "alpha ", round[1].Content)
	assert.Equal(t, "user", round[2].Role)
	assert.Equal(t, lengthContinueInstruction, round[2].Content)
	assert.Equal(t, "beta", res.Resp.Content)

	// No SSE events between the attempts' token runs — the persistence
	// single-row invariant (spec: SSE 与持久化零改动) holds because the
	// helper emits nothing for the content-only continuation.
	assert.Empty(t, findEvents(events, stream.EventToolCall))
	assert.Empty(t, findEvents(events, stream.EventAgentError))
}

// The empty truncation (thinking consumed the budget) takes the same path.
func TestRunLLMRound_EmptyLengthContinues(t *testing.T) {
	client := newFake(
		fakeResponse{reasoningContent: "thinking...", finishReason: "length"},
		fakeResponse{content: "answer", finishReason: "stop"},
	)
	round := []Message{{Role: "user", Content: "q"}}
	req := LLMRequest{Model: "m", Messages: round, MaxCompletionTokens: 800}

	res, err := runLLMRound(context.Background(), client, stream.NewHub(64), "Liang", req, &round,
		enabledPolicy(512, 3), config.AgentRetryConfig{}, func(r []Message) []Message { return r }, nil,
		func(string) error { return nil }, func(string) error { return nil })
	require.NoError(t, err)
	assert.Equal(t, 2, client.requestCount())
	assert.Equal(t, "answer", res.Content)
	// The empty assistant message is wire-legal scaffolding, then the
	// instruction follows.
	assert.Equal(t, "assistant", round[1].Role)
	assert.Empty(t, round[1].Content)
	assert.Equal(t, "thinking...", round[1].ReasoningContent)
	assert.Equal(t, "user", round[2].Role)
}

// Disabled policy: one attempt only, the length fact is reported, and no
// scaffold is appended — byte-for-byte prior loop behavior.
func TestRunLLMRound_DisabledSingleAttempt(t *testing.T) {
	client := newFake(fakeResponse{content: "cut", finishReason: "length"})
	round := []Message{{Role: "user", Content: "q"}}
	req := LLMRequest{Model: "m", Messages: round, MaxCompletionTokens: 100}

	res, err := runLLMRound(context.Background(), client, stream.NewHub(64), "Liang", req, &round,
		config.LengthContinueConfig{}, config.AgentRetryConfig{}, func(r []Message) []Message { return r }, nil,
		func(string) error { return nil }, func(string) error { return nil })
	require.NoError(t, err)
	assert.Equal(t, 1, client.requestCount())
	assert.True(t, res.LengthHit)
	assert.Equal(t, "cut", res.Content)
	assert.Len(t, round, 1, "disabled policy must not scaffold")
}

// Exhaustion: max_retries continuations, still length on the final attempt.
func TestRunLLMRound_Exhaustion(t *testing.T) {
	client := newFake(
		fakeResponse{content: "a", finishReason: "length"},
		fakeResponse{content: "b", finishReason: "length"},
		fakeResponse{content: "c", finishReason: "length"},
	)
	round := []Message{{Role: "user", Content: "q"}}
	req := LLMRequest{Model: "m", Messages: round, MaxCompletionTokens: 8192}

	res, err := runLLMRound(context.Background(), client, stream.NewHub(64), "Liang", req, &round,
		enabledPolicy(8192, 2), config.AgentRetryConfig{}, func(r []Message) []Message { return r }, nil,
		func(string) error { return nil }, func(string) error { return nil })
	require.NoError(t, err)

	assert.Equal(t, 3, client.requestCount()) // initial + 2 continuations
	assert.Equal(t, []int{8192, 16384, 24576}[2], client.calls[2].MaxCompletionTokens)
	assert.True(t, res.LengthHit)
	assert.Equal(t, "abc", res.Content, "accumulated content survives exhaustion")
}

// tool_calls truncation triage (B-call-1, design D5): parseable calls
// dispatch through the callback; truncated calls get the synthetic
// not-executed result plus the sanitized SSE pair.
func TestRunLLMRound_ToolCallsTriage(t *testing.T) {
	valid := ToolCall{ID: "call_ok", Function: ToolCallFunction{Name: "xizhi_write_file", Arguments: `{"path":"a.txt","content":"hi"}`}}
	truncated := ToolCall{ID: "call_cut", Function: ToolCallFunction{Name: "xizhi_write_file", Arguments: `{"path":"b.txt","content":"def main(`}}
	client := newFake(
		fakeResponse{content: "preamble", finishReason: "length", toolCalls: []ToolCall{valid, truncated}},
		fakeResponse{content: "done", finishReason: "stop"},
	)
	round := []Message{{Role: "user", Content: "q"}}
	req := LLMRequest{Model: "m", Messages: round, MaxCompletionTokens: 100}

	var dispatched []ToolCall
	var res roundResult
	var err error
	events := collectEvents(t, func(hub *stream.Hub) {
		res, err = runLLMRound(context.Background(), client, hub, "Chongzhi", req, &round,
			enabledPolicy(512, 3), config.AgentRetryConfig{}, func(r []Message) []Message { return r },
			// The production closure is executeAndRecordToolCalls; the stub
			// mirrors its round effect (append each call's answer) so the
			// wire-protocol assertion below is meaningful.
			func(_ context.Context, calls []ToolCall) {
				dispatched = append(dispatched, calls...)
				for _, tc := range calls {
					round = append(round, Message{Role: "tool", Content: "ok", ToolCallID: tc.ID, Name: tc.Function.Name})
				}
			},
			func(string) error { return nil }, func(string) error { return nil })
	})
	require.NoError(t, err)

	// Only the parseable call dispatched.
	require.Len(t, dispatched, 1)
	assert.Equal(t, "call_ok", dispatched[0].ID)

	// Round shape (design D5 / the B-call-1 wire contract, task 6.6):
	// assistant carries BOTH calls verbatim (half-truncated args included),
	// the truncated call has a synthetic tool answer BEFORE the continuation
	// request, and the dispatched call's answer is appended by the callback.
	require.GreaterOrEqual(t, len(round), 3)
	assistant := round[1]
	assert.Equal(t, "assistant", assistant.Role)
	require.Len(t, assistant.ToolCalls, 2)
	assert.Equal(t, truncated.Function.Arguments, assistant.ToolCalls[1].Function.Arguments, "half-truncated args stay in context verbatim")
	assert.Equal(t, "preamble", assistant.Content)

	toolMsgs := map[string]Message{}
	for _, m := range round[2:] {
		if m.Role == "tool" {
			toolMsgs[m.ToolCallID] = m
		}
	}
	require.Contains(t, toolMsgs, "call_cut")
	assert.Contains(t, toolMsgs["call_cut"].Content, "not executed")
	assert.Contains(t, toolMsgs["call_cut"].Content, "truncated by the output length limit")

	// Every tool_call of the assistant message is answered before the next
	// request (wire protocol invariant).
	answered := map[string]bool{}
	for _, m := range round {
		if m.Role == "tool" {
			answered[m.ToolCallID] = true
		}
	}
	for _, tc := range assistant.ToolCalls {
		assert.True(t, answered[tc.ID], "tool_call %s must have a tool answer before the continuation request", tc.ID)
	}

	// The sanitized event pair for the truncated call: args are {} so the SSE
	// payload stays parseable.
	cutCalls := findEvents(events, stream.EventToolCall)
	var sawCut bool
	for _, e := range cutCalls {
		if e.Meta[stream.MetaToolCallID] == "call_cut" {
			sawCut = true
			args, ok := e.Meta[stream.MetaArgs].(json.RawMessage)
			require.True(t, ok, "tool_call event must carry args")
			assert.JSONEq(t, "{}", string(args))
		}
	}
	assert.True(t, sawCut, "truncated call must emit a tool_call event")
	var sawCutResult bool
	for _, e := range findEvents(events, stream.EventToolResult) {
		if e.Meta[stream.MetaToolCallID] == "call_cut" {
			sawCutResult = true
		}
	}
	assert.True(t, sawCutResult, "truncated call must emit a tool_result event")
	_ = res
}

// Empty-string arguments route to the synthetic path (no registry tool can
// run without args).
func TestRunLLMRound_EmptyArgsRouteToSynthetic(t *testing.T) {
	empty := ToolCall{ID: "call_empty", Function: ToolCallFunction{Name: "xizhi_read_file", Arguments: ""}}
	client := newFake(
		fakeResponse{finishReason: "length", toolCalls: []ToolCall{empty}},
		fakeResponse{content: "done", finishReason: "stop"},
	)
	round := []Message{{Role: "user", Content: "q"}}

	var dispatched []ToolCall
	events := collectEvents(t, func(hub *stream.Hub) {
		_, err := runLLMRound(context.Background(), client, hub, "Chongzhi",
			LLMRequest{Model: "m", Messages: round, MaxCompletionTokens: 100}, &round,
			enabledPolicy(512, 3), config.AgentRetryConfig{}, func(r []Message) []Message { return r },
			// The production closure is executeAndRecordToolCalls; the stub
			// mirrors its round effect (append each call's answer) so the
			// wire-protocol assertion below is meaningful.
			func(_ context.Context, calls []ToolCall) {
				dispatched = append(dispatched, calls...)
				for _, tc := range calls {
					round = append(round, Message{Role: "tool", Content: "ok", ToolCallID: tc.ID, Name: tc.Function.Name})
				}
			},
			func(string) error { return nil }, func(string) error { return nil })
		require.NoError(t, err)
	})
	assert.Empty(t, dispatched)
	var sawSynthetic bool
	for _, m := range round {
		if m.Role == "tool" && m.ToolCallID == "call_empty" {
			sawSynthetic = true
		}
	}
	assert.True(t, sawSynthetic)
	assert.NotEmpty(t, findEvents(events, stream.EventToolCall))
}

// A mid-continuation StreamChat error surfaces as the caller's ordinary
// llm_error — the loop does not swallow it into the exhaustion path.
func TestRunLLMRound_ErrorMidContinuation(t *testing.T) {
	client := newFake(
		fakeResponse{content: "a", finishReason: "length"},
		fakeResponse{err: context.DeadlineExceeded},
	)
	round := []Message{{Role: "user", Content: "q"}}
	_, err := runLLMRound(context.Background(), client, stream.NewHub(64), "Liang",
		LLMRequest{Model: "m", Messages: round, MaxCompletionTokens: 100}, &round,
		enabledPolicy(512, 3), config.AgentRetryConfig{}, func(r []Message) []Message { return r }, nil,
		func(string) error { return nil }, func(string) error { return nil })
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// The exhaustion error text must never classify as transient — a sub-agent's
// length exhaustion must not enter the transient retry pipeline.
func TestLengthExhaustedMessage_NotTransient(t *testing.T) {
	msg := lengthExhaustedMessage("Chongzhi", 3, 32768)
	assert.False(t, isTransientError(assert.AnError))
	assert.False(t, isTransientError(errString(msg)), "length-exhausted error must not match transient signals: %s", msg)
}

// --- agent-loop integration (adoption at the four call sites) ----------------

// testLiangCfg carries NO quota: since per-model-completion-budget the output
// quota and continuation policy both ride the turn's ModelOverride (resolved
// from the catalog entry), so the agent config holds only capability fields.
func testLiangCfg() config.AgentConfig {
	return config.AgentConfig{Name: "Liang", SystemPrompt: "sys", MaxRounds: 10}
}

// newTestLiang builds a Liang whose turn resolves to a catalog entry with the
// given quota and continuation policy (the entry-driven form of the old
// cfg.max_tokens + global lc constructor pair).
func newTestLiang(t *testing.T, client LLMClient, quota int, lc config.LengthContinueConfig) *Liang {
	t.Helper()
	l, err := NewLiang(testLiangCfg(), client, tool.NewRegistry(),
		ModelOverride{Model: "m", MaxCompletionTokens: quota, LengthContinue: lc})
	require.NoError(t, err)
	return l
}

// Liang, exhaustion: agent_error(length_exhausted) + agent_end + non-nil
// error + accumulated finalContent (design D7).
func TestLiangRun_LengthExhausted(t *testing.T) {
	client := newFake(
		fakeResponse{content: "part1", finishReason: "length"},
		fakeResponse{content: "part2", finishReason: "length"},
		fakeResponse{content: "part3", finishReason: "length"},
		fakeResponse{content: "part4", finishReason: "length"},
	)
	l := newTestLiang(t, client, 8192, enabledPolicy(8192, 3))

	var content string
	var runErr error
	events := collectEvents(t, func(hub *stream.Hub) {
		content, _, _, runErr = l.Run(context.Background(), []Message{{Role: "user", Content: "q"}}, hub)
	})
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "length")
	assert.Equal(t, "part1part2part3part4", content)

	errs := findEvents(events, stream.EventAgentError)
	require.Len(t, errs, 1)
	assert.Equal(t, "length_exhausted", errs[0].Meta[stream.MetaCode])
	ends := findEvents(events, stream.EventAgentEnd)
	assert.NotEmpty(t, ends)
}

// Confucius: a length round continues and the turn succeeds with accumulated
// content; context_tokens observes the FINAL attempt only (design D8).
func TestConfuciusRun_LengthContinuation(t *testing.T) {
	client := newFake(
		fakeResponse{content: "first half ", tokens: []string{"first half "}, finishReason: "length", usage: Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}},
		fakeResponse{content: "second half", tokens: []string{"second half"}, finishReason: "stop", usage: Usage{PromptTokens: 120, CompletionTokens: 30, TotalTokens: 150}},
	)
	cfg := config.AgentConfig{Name: "Confucius", SystemPrompt: "sys"}
	subAgents := map[string]SubAgentFactory{
		ToolInvokeChongzhi: func() (Agent, error) { return &fakeAgent{name: "Chongzhi"}, nil },
		ToolInvokeLiang:    func() (Agent, error) { return &fakeAgent{name: "Liang"}, nil },
	}
	c, err := NewConfucius(cfg, client, tool.NewRegistry(), subAgents,
		ModelOverride{Model: "m", MaxCompletionTokens: 8192, LengthContinue: enabledPolicy(8192, 3)})
	require.NoError(t, err)

	var content string
	var breakdown *TurnBreakdown
	var runErr error
	events := collectEvents(t, func(hub *stream.Hub) {
		content, _, breakdown, runErr = c.Run(context.Background(), []Message{{Role: "user", Content: "q"}}, hub)
	})
	require.NoError(t, runErr)
	assert.Equal(t, "first half second half", content)

	// Expansion arithmetic visible on the wire (llm_raw_log parity).
	require.Equal(t, 2, client.requestCount())
	assert.Equal(t, 8192, client.calls[0].MaxCompletionTokens)
	assert.Equal(t, 16384, client.calls[1].MaxCompletionTokens)

	// Usage sums both attempts; context_tokens is the FINAL attempt's
	// prompt+completion, never the cross-attempt sum (design D8).
	require.NotNil(t, breakdown)
	assert.Equal(t, 300, breakdown.ByAgent["Confucius"].TotalTokens)
	assert.Equal(t, 150, breakdown.LastRoundContextTokens)

	// Seamless stream: the token events of the two attempts are consecutive
	// with nothing between them (MergeEvents single-row invariant).
	var tokenIdx []int
	for i, e := range events {
		if e.Type == stream.EventToken {
			tokenIdx = append(tokenIdx, i)
		}
	}
	require.Len(t, tokenIdx, 2)
	assert.Equal(t, tokenIdx[1], tokenIdx[0]+1, "no event may sit between continuation token runs")

	// No error events on the success path.
	assert.Empty(t, findEvents(events, stream.EventAgentError))
}

// Continuation attempts do not consume max_rounds: a max_rounds=1 agent that
// continues once and then terminates naturally does NOT hit the cap path.
func TestLiangRun_ContinuationDoesNotConsumeRounds(t *testing.T) {
	client := newFake(
		fakeResponse{content: "a", finishReason: "length"},
		fakeResponse{content: "b", finishReason: "stop"},
	)
	l := newTestLiang(t, client, 8192, enabledPolicy(8192, 3))
	l.maxRounds = 1

	var runErr error
	events := collectEvents(t, func(hub *stream.Hub) {
		_, _, _, runErr = l.Run(context.Background(), []Message{{Role: "user", Content: "q"}}, hub)
	})
	require.NoError(t, runErr, "natural stop after continuation must not trip the round cap")
	assert.Empty(t, findEvents(events, stream.EventAgentError))
	assert.False(t, l.LastRunHitCap())
}

// Wrap-up rounds continue too; a final-attempt length exhaustion routes onto
// the caller's round_cap_exhausted path (design Risks).
func TestLiangRun_WrapUpContinuesOnLength(t *testing.T) {
	// Round 1 emits a tool_call (dispatched), round 2 is the last allowed
	// round and also emits a tool_call -> cap -> wrap-up, whose first attempt
	// is length and whose continuation answers.
	reg := tool.NewRegistry()
	xizhi.RegisterAll(reg, t.TempDir(), testXizhiConfigLike())
	client := newFake(
		fakeResponse{finishReason: "tool_calls", toolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunction{Name: xizhi.NameReadFile, Arguments: `{"path":"x.txt"}`}}}},
		fakeResponse{finishReason: "tool_calls", toolCalls: []ToolCall{{ID: "c2", Function: ToolCallFunction{Name: xizhi.NameReadFile, Arguments: `{"path":"y.txt"}`}}}},
		fakeResponse{content: "wrap ", finishReason: "length"},
		fakeResponse{content: "answer", finishReason: "stop"},
	)
	cfg := testLiangCfg()
	cfg.MaxRounds = 2
	cfg.Tools = []string{xizhi.NameReadFile}
	l, err := NewLiang(cfg, client, reg,
		ModelOverride{Model: "m", MaxCompletionTokens: 8192, LengthContinue: enabledPolicy(8192, 3)})
	require.NoError(t, err)

	var content string
	var runErr error
	events := collectEvents(t, func(hub *stream.Hub) {
		content, _, _, runErr = l.Run(context.Background(), []Message{{Role: "user", Content: "q"}}, hub)
	})
	require.NoError(t, runErr)
	assert.Equal(t, "wrap answer", content)
	// 2 loop rounds + 2 wrap attempts.
	assert.Equal(t, 4, client.requestCount())
	assert.Empty(t, findEvents(events, stream.EventAgentError))
}

// --- write-budget guidance injection (design D10, task 5.2) ------------------

func writeToolsRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry()
	xizhi.RegisterAll(reg, t.TempDir(), testXizhiConfigLike())
	return reg
}

// testXizhiConfigLike mirrors the xizhi test helper (enabled read/write/
// modify) without importing the xizhi test package.
func testXizhiConfigLike() config.XizhiConfig {
	return config.XizhiConfig{
		Read:   config.XizhiToolConfig{Enabled: true},
		Write:  config.XizhiToolConfig{Enabled: true},
		Modify: config.XizhiToolConfig{Enabled: true},
	}
}

func TestBuildRegularToolsJSON_InjectsPerEntryWriteBudget(t *testing.T) {
	reg := writeToolsRegistry(t)
	names := []string{xizhi.NameReadFile, xizhi.NameWriteFile, xizhi.NameModifyFile}

	raw, err := buildRegularToolsJSON(reg, names, 8192)
	require.NoError(t, err)
	var tools openAIToolList
	require.NoError(t, json.Unmarshal(raw, &tools))

	byName := map[string]openAIToolFunc{}
	for _, tl := range tools {
		byName[tl.Function.Name] = tl.Function
	}
	require.Contains(t, byName, xizhi.NameWriteFile)
	require.Contains(t, byName, xizhi.NameModifyFile)

	// floor(8192 × 0.7) = 5734 appears in both write-family descriptions,
	// with the chunked-writes steering.
	for _, name := range []string{xizhi.NameWriteFile, xizhi.NameModifyFile} {
		assert.Contains(t, byName[name].Description, "5734", "%s description must carry the turn-resolved entry budget", name)
		assert.Contains(t, byName[name].Description, "multiple smaller writes", "%s description must keep the chunking pattern advice", name)
	}

	// A non-write tool is untouched, and the static (registry) description
	// itself carries the append-mode pattern advice without a number.
	assert.NotContains(t, byName[xizhi.NameReadFile].Description, "5734")
	var static string
	for _, spec := range reg.List() {
		if spec.Name == xizhi.NameWriteFile {
			static = spec.Description
		}
	}
	require.NotEmpty(t, static)
	assert.Contains(t, static, "append")
	assert.NotContains(t, static, "5734", "the static registry description must not hardcode a per-entry number")
}

// TestBuildRegularToolsJSON_BudgetVariesByEntryAndSkipsZero: the budget
// number follows the turn-resolved catalog entry (per-model-completion-budget
// D4 — 8192→5734, 4096→2867), and an unresolved quota (0) skips injection.
func TestBuildRegularToolsJSON_BudgetVariesByEntryAndSkipsZero(t *testing.T) {
	reg := writeToolsRegistry(t)
	names := []string{xizhi.NameWriteFile}

	for _, tc := range []struct{ quota, budget int }{
		{8192, 5734}, // floor(8192 × 0.7)
		{4096, 2867}, // floor(4096 × 0.7) — a different catalog entry
	} {
		raw, err := buildRegularToolsJSON(reg, names, tc.quota)
		require.NoError(t, err)
		var tools openAIToolList
		require.NoError(t, json.Unmarshal(raw, &tools))
		require.Len(t, tools, 1)
		assert.Contains(t, tools[0].Function.Description, fmt.Sprintf("~%d tokens", tc.budget),
			"quota %d must steer a %d-token budget", tc.quota, tc.budget)
	}

	// Quota unresolved: no number to steer by — no injection.
	raw, err := buildRegularToolsJSON(reg, names, 0)
	require.NoError(t, err)
	tools := openAIToolList(nil)
	require.NoError(t, json.Unmarshal(raw, &tools))
	assert.NotContains(t, tools[0].Function.Description, "Write budget:")
}

// TestLiangRun_PerEntryContinuationSplit: two turns of one deployment select
// two catalog entries — A with length_continue configured, B without — and
// only the A turn continues; the B turn keeps the disabled behavior
// byte-for-byte (per-model-completion-budget D3: per-entry opt-in).
func TestLiangRun_PerEntryContinuationSplit(t *testing.T) {
	// Entry A's turn: length → continuation with the expanded budget.
	clientA := newFake(
		fakeResponse{content: "first ", finishReason: "length"},
		fakeResponse{content: "rest", finishReason: "stop"},
	)
	l := newTestLiang(t, clientA, 8192, enabledPolicy(8192, 3))
	var contentA string
	var runErrA error
	collectEvents(t, func(hub *stream.Hub) {
		contentA, _, _, runErrA = l.Run(context.Background(), []Message{{Role: "user", Content: "q"}}, hub)
	})
	require.NoError(t, runErrA)
	assert.Equal(t, "first rest", contentA)
	require.Equal(t, 2, clientA.requestCount())
	assert.Equal(t, 8192, clientA.calls[0].MaxCompletionTokens)
	assert.Equal(t, 8192+8192, clientA.calls[1].MaxCompletionTokens)

	// Entry B's turn (same deployment, no length_continue): the length round
	// terminates the run silently — one attempt, content kept, no error.
	clientB := newFake(
		fakeResponse{content: "cut off", finishReason: "length"},
	)
	l2 := newTestLiang(t, clientB, 4096, config.LengthContinueConfig{})
	var contentB string
	var runErrB error
	events := collectEvents(t, func(hub *stream.Hub) {
		contentB, _, _, runErrB = l2.Run(context.Background(), []Message{{Role: "user", Content: "q"}}, hub)
	})
	require.NoError(t, runErrB, "a disabled entry must keep the silent-terminal behavior")
	assert.Equal(t, "cut off", contentB)
	assert.Equal(t, 1, clientB.requestCount())
	assert.Equal(t, 4096, clientB.calls[0].MaxCompletionTokens)
	assert.Empty(t, findEvents(events, stream.EventAgentError))
}

// errString wraps a plain string as an error (test helper).
func errString(s string) error { return &stringError{s} }

type stringError struct{ s string }

func (e *stringError) Error() string { return e.s }

var _ = strings.TrimSpace
