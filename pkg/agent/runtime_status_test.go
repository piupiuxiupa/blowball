package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/config"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/trace"
	"github.com/lush/blowball/pkg/stream"
	"github.com/lush/blowball/pkg/tool"
)

type fakeAgentRuntimeStatusStore struct {
	entries        []model.RuntimeStatus
	clearedEntries []model.RuntimeStatus
	appendErr      error
	clearCalls     int
}

func (f *fakeAgentRuntimeStatusStore) AppendRuntimeStatus(_ context.Context, _ string, status model.RuntimeStatus) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	f.entries = append(f.entries, status)
	return nil
}

func (f *fakeAgentRuntimeStatusStore) ListRuntimeStatus(_ context.Context, _ string) ([]model.RuntimeStatus, error) {
	return append([]model.RuntimeStatus(nil), f.entries...), nil
}

func (f *fakeAgentRuntimeStatusStore) ClearRuntimeStatus(_ context.Context, _ string) error {
	f.clearCalls++
	f.clearedEntries = append(f.clearedEntries, f.entries...)
	f.entries = nil
	return nil
}

func TestConfucius_CurrentStatusAppendsRedisEntryBeforeUser(t *testing.T) {
	store := &fakeAgentRuntimeStatusStore{}
	client := newFake(fakeResponse{content: "done", finishReason: "stop"})
	c := newTestConfucius(t, client, nil)
	ctx := WithSessionID(trace.WithContext(context.Background(), "trace-1"), "s-1")
	ctx = WithRuntimeStatusStore(ctx, store)

	_, content, _, _, err := runConfuciusAndCollectContext(t, c, []Message{{Role: "user", Content: "go"}}, ctx)
	require.NoError(t, err)
	assert.Equal(t, "done", content)
	require.Len(t, store.entries, 1)
	entry := store.entries[0]
	assert.Equal(t, "trace-1:1:current_status", entry.StatusID)
	assert.Equal(t, RuntimeStatusCurrent, entry.Kind)
	assert.Equal(t, int64(1), entry.Seq)
	assert.Equal(t, "trace-1:0", entry.BeforeClientMsgID)
	assert.Empty(t, entry.AfterToolCallID)
	assert.Equal(t, client.calls[0].Messages[1].Content, entry.Content)
}

func TestConfucius_RuntimeStatusAppendFailureDoesNotFailTurn(t *testing.T) {
	store := &fakeAgentRuntimeStatusStore{appendErr: errors.New("redis down")}
	client := newFake(fakeResponse{content: "done", finishReason: "stop"})
	c := newTestConfucius(t, client, nil)
	ctx := WithSessionID(trace.WithContext(context.Background(), "trace-1"), "s-1")
	ctx = WithRuntimeStatusStore(ctx, store)

	_, content, _, _, err := runConfuciusAndCollectContext(t, c, []Message{{Role: "user", Content: "go"}}, ctx)
	require.NoError(t, err)
	assert.Equal(t, "done", content)
	assert.Empty(t, store.entries)
}

func TestRuntimeStatus_ParallelBudgetWaitsForCompleteGroup(t *testing.T) {
	for _, name := range []string{"root", "subagent"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			slowStarted := make(chan struct{})
			fastFinished := make(chan struct{})
			releaseSlow := make(chan struct{})
			var release sync.Once
			reg := tool.NewRegistry()
			require.NoError(t, reg.Register(&tool.ToolSpec{
				Name: "slow", Description: "slow tool", ParametersJSON: json.RawMessage(`{"type":"object"}`),
				Execute: func(ctx context.Context, _ json.RawMessage) (any, error) {
					close(slowStarted)
					select {
					case <-releaseSlow:
						return "slow result", nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				},
			}))
			require.NoError(t, reg.Register(&tool.ToolSpec{
				Name: "fast", Description: "fast tool", ParametersJSON: json.RawMessage(`{"type":"object"}`),
				Execute: func(ctx context.Context, _ json.RawMessage) (any, error) {
					select {
					case <-slowStarted:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					close(fastFinished)
					return "fast result", nil
				},
			}))
			client := newFake(
				fakeResponse{finishReason: "tool_calls", toolCalls: []ToolCall{
					{ID: "slow-call", Function: ToolCallFunction{Name: "slow", Arguments: `{}`}},
					{ID: "fast-call", Function: ToolCallFunction{Name: "fast", Arguments: `{}`}},
				}},
				fakeResponse{content: "done", finishReason: "stop"},
			)
			cfg := config.AgentConfig{Name: "Confucius", SystemPrompt: "test", Tools: []string{"slow", "fast"}, MaxRounds: 5}
			var runner Agent
			var err error
			if name == "root" {
				runner, err = NewConfucius(cfg, client, reg, testSpawnCoordinator(reg, nil), testTurn())
			} else {
				cfg.Name = "Subagent"
				runner, err = newGenericFromAgentConfig(cfg, client, reg, testTurn())
			}
			require.NoError(t, err)
			store := &fakeAgentRuntimeStatusStore{}
			ctx = WithRuntimeStatusStore(WithSessionID(trace.WithContext(ctx, "parallel"), "s-1"), store)
			hub := stream.NewHub(128)
			done := make(chan error, 1)
			var wg sync.WaitGroup
			wg.Go(func() {
				_, _, _, err := runner.Run(ctx, []Message{{Role: "user", Content: "work"}}, hub)
				done <- err
			})
			t.Cleanup(func() {
				cancel()
				release.Do(func() { close(releaseSlow) })
				wg.Wait()
				hub.Close()
			})

			select {
			case <-fastFinished:
			case <-ctx.Done():
				t.Fatal("both tools must start concurrently while the first call is blocked")
			}
			assert.Equal(t, 1, client.requestCount())
			select {
			case <-done:
				t.Fatal("the turn finished before the slow tool result")
			case <-time.After(30 * time.Millisecond):
			}
			assert.Equal(t, 1, client.requestCount(), "no follow-up request while one result is still pending")
			release.Do(func() { close(releaseSlow) })
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal("turn did not finish after both tool results")
			}
			wg.Wait()
			require.Equal(t, 2, client.requestCount())
			first, second := client.calls[0].Messages, client.calls[1].Messages
			require.GreaterOrEqual(t, len(second), len(first))
			assert.Equal(t, first, second[:len(first)])
			suffix := second[len(first):]
			require.Len(t, suffix, 4)
			assert.Len(t, suffix[0].ToolCalls, 2)
			assert.Equal(t, "slow-call", suffix[1].ToolCallID)
			assert.Equal(t, "fast-call", suffix[2].ToolCallID)
			assert.Contains(t, suffix[1].Content, "slow result")
			assert.Contains(t, suffix[2].Content, "fast result")
			assert.Equal(t, RuntimeStatusToolBudget, suffix[3].RuntimeStatus)
			assert.Contains(t, suffix[3].Content, "rounds_used: 1")
			assert.Contains(t, suffix[3].Content, "rounds_remaining: 4")
			if name == "root" {
				require.Len(t, store.entries, 2)
				assert.Equal(t, "fast-call", store.entries[1].AfterToolCallID,
					"the anchor follows call order, not execution completion order")
			} else {
				assert.Empty(t, store.entries, "subagent status must not enter the root Redis log")
				for _, message := range runner.(*SubAgent).LastRunMessages() {
					assert.Empty(t, message.RuntimeStatus, "subagent durable transcript must be status-free")
				}
			}
			hub.Close()
			for len(hub.Events()) > 0 {
				raw, err := json.Marshal(<-hub.Events())
				require.NoError(t, err)
				assert.NotContains(t, string(raw), "tool_budget", "runtime status must not become an SSE event")
				assert.NotContains(t, string(raw), "current_status", "runtime status must not become an SSE event")
			}
		})
	}
}

func TestNewCurrentStatusForRequest_UsesConservativeCompleteEstimate(t *testing.T) {
	timestamp := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	messages := []Message{
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "current"},
	}
	budget := NewRequestBudget(20000, 1000)
	status := newCurrentStatusForRequest("system prompt", messages, []byte(`[{"type":"function"}]`), timestamp, budget)

	assert.Equal(t, RuntimeStatusCurrent, status.RuntimeStatus)
	estimated := runtimeStatusIntField(t, status.Content, "estimated_input_tokens")
	inputBudget := runtimeStatusIntField(t, status.Content, "input_budget_tokens")
	remaining := runtimeStatusIntField(t, status.Content, "estimated_remaining_tokens")
	assert.Positive(t, estimated)
	assert.Equal(t, budget.InputTokens, inputBudget)

	actual := EstimateRenderedRequest("system prompt", withInitialCurrentStatus(messages, status), []byte(`[{"type":"function"}]`)).Tokens
	assert.GreaterOrEqual(t, estimated, actual, "measurement status must conservatively cover the sent status")
	assert.LessOrEqual(t, remaining, max(budget.InputTokens-actual, 0),
		"reported remaining must not overstate the final request's available input")
}

func TestNewCurrentStatusForRequest_WithoutValidBudgetOmitsUsage(t *testing.T) {
	timestamp := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	status := newCurrentStatusForRequest("system", []Message{{Role: "user", Content: "go"}}, nil, timestamp, RequestBudget{})
	assert.Contains(t, status.Content, timestamp.Format(time.RFC3339))
	assert.NotContains(t, status.Content, "context_usage")
}

func TestConfucius_RuntimeStatusCountsTowardPreflightBudget(t *testing.T) {
	client := newFake()
	c := newTestConfucius(t, client, nil)
	canonical := []Message{{Role: "user", Content: "go"}}
	base := EstimateRenderedRequest(c.cfg.SystemPrompt, canonical, c.toolsJSON).Tokens
	c.turn.MaxCompletionTokens = 100
	c.turn.MaxContextTokens = base + 1 + c.turn.MaxCompletionTokens + 4096

	_, _, _, _, err := runConfuciusAndCollect(t, c, canonical)
	var budgetErr *RequestTooLargeError
	assert.ErrorAs(t, err, &budgetErr, "the runtime status itself must exceed the remaining input budget")
	assert.Zero(t, client.requestCount())
}

func TestConfucius_ToolBudgetIncludedInNextRequestEstimate(t *testing.T) {
	client := newFake(
		fakeResponse{finishReason: "tool_calls", toolCalls: []ToolCall{{
			ID: "spawn-1", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: `{"task":"work"}`},
		}}},
		fakeResponse{content: "done", finishReason: "stop"},
	)
	c := newTestConfucius(t, client, map[string]Agent{
		SpawnSubagentTool: &fakeAgent{name: "child", content: "child result"},
	})
	var appended []Message
	c.SetRoundRequestEstimator(func(_ context.Context, usage Usage, messages []Message) ContextRequestEstimate {
		appended = append([]Message(nil), messages...)
		return EstimateRequestFromUsage(usage.PromptTokens, usage.CompletionTokens, messages)
	})

	_, _, _, _, err := runConfuciusAndCollect(t, c, []Message{{Role: "user", Content: "go"}})
	require.NoError(t, err)
	require.NotEmpty(t, appended)
	assert.Equal(t, RuntimeStatusToolBudget, appended[len(appended)-1].RuntimeStatus)
}

func TestConfucius_CompactionStartsNewRuntimeStatusEpoch(t *testing.T) {
	messages := []Message{{Role: "user", Content: "go"}}
	client := newFake(fakeResponse{content: "done", finishReason: "stop"})
	c := newTestConfucius(t, client, nil)
	probe := newCurrentStatusForRequest(c.cfg.SystemPrompt, messages, c.toolsJSON, time.Now(), NewRequestBudget(20000, 100))
	initialEstimate := EstimateRenderedRequest(c.cfg.SystemPrompt, withInitialCurrentStatus(messages, probe), c.toolsJSON).Tokens
	c.turn.MaxCompletionTokens = 100
	c.turn.MaxContextTokens = initialEstimate + c.turn.MaxCompletionTokens + 4096 + 100
	c.SetRoundHook(func(context.Context, Usage) []Message {
		return []Message{{Role: "user", Content: "summary"}}
	})
	store := &fakeAgentRuntimeStatusStore{}
	ctx := WithSessionID(trace.WithContext(context.Background(), "trace-compact"), "s-1")
	ctx = WithRuntimeStatusStore(ctx, store)

	_, _, _, _, err := runConfuciusAndCollectContext(t, c, messages, ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, store.clearCalls)
	require.NotEmpty(t, store.clearedEntries)
	oldStatus := store.clearedEntries[0].Content
	require.Len(t, client.calls, 1)
	statusCount := 0
	for _, message := range client.calls[0].Messages {
		assert.NotEqual(t, oldStatus, message.Content, "the pre-compaction status must not survive")
		if message.RuntimeStatus == RuntimeStatusCurrent {
			statusCount++
			assert.Contains(t, message.Content, "context_usage")
			assert.NotContains(t, message.Content, "estimated_remaining_tokens: -")
		}
	}
	assert.Equal(t, 1, statusCount, "the compacted projection must receive one fresh current status")
}

func TestSubAgent_RuntimeStatusAppendOnlyAndSnapshotExcluded(t *testing.T) {
	fake := &fakeExecutor{name: "fake_tool", result: "ok"}
	reg := newSubAgentRegistryWithFake(t, fake)
	client := newFake(
		fakeResponse{finishReason: "tool_calls", toolCalls: []ToolCall{{
			ID: "call-1", Function: ToolCallFunction{Name: fake.name, Arguments: `{}`},
		}}},
		fakeResponse{content: "done", finishReason: "stop"},
	)
	sub := newTestSubAgent(t, client, reg)
	sub.maxRounds = 5

	_, content, _, err := runSubAgentAndCollect(t, sub, []Message{{Role: "user", Content: "work"}})
	require.NoError(t, err)
	assert.Equal(t, "done", content)
	require.Len(t, client.calls, 2)
	first := client.calls[0].Messages
	second := client.calls[1].Messages
	require.Greater(t, len(second), len(first))
	assert.Equal(t, first, second[:len(first)])
	last := second[len(second)-1]
	assert.Equal(t, RuntimeStatusToolBudget, last.RuntimeStatus)
	assert.Contains(t, last.Content, "rounds_used: 1")
	assert.Contains(t, last.Content, "rounds_remaining: 4")
	for _, message := range sub.LastRunMessages() {
		assert.Empty(t, message.RuntimeStatus, "durable SubAgent snapshots must exclude runtime status")
	}
}

func TestSubAgent_ToolBudgetUsesInstanceMaxRounds(t *testing.T) {
	tests := []struct {
		name      string
		maxRounds int
		remaining string
	}{
		{name: "two rounds", maxRounds: 2, remaining: "1"},
		{name: "five rounds", maxRounds: 5, remaining: "4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeExecutor{name: "fake_tool", result: "ok"}
			reg := newSubAgentRegistryWithFake(t, fake)
			client := newFake(
				fakeResponse{finishReason: "tool_calls", toolCalls: []ToolCall{{
					ID: "call-1", Function: ToolCallFunction{Name: fake.name, Arguments: `{}`},
				}}},
				fakeResponse{content: "done", finishReason: "stop"},
			)
			sub := newTestSubAgent(t, client, reg)
			sub.maxRounds = tt.maxRounds
			_, _, _, err := runSubAgentAndCollect(t, sub, []Message{{Role: "user", Content: "work"}})
			require.NoError(t, err)
			require.Len(t, client.calls, 2)
			messages := client.calls[1].Messages
			last := messages[len(messages)-1]
			assert.Equal(t, tt.maxRounds, sub.maxRounds)
			assert.Contains(t, last.Content, "max_rounds: "+strconv.Itoa(tt.maxRounds))
			assert.Contains(t, last.Content, "rounds_remaining: "+tt.remaining)
		})
	}
}

func TestSubAgent_TerminalResponseFormatIgnoresTrailingRuntimeStatus(t *testing.T) {
	format := json.RawMessage(`{"type":"json_schema"}`)
	sub := &SubAgent{responseFormat: format, toolsIsNotNil: true}
	got, ok := sub.terminalResponseFormat([]Message{
		{Role: "tool", ToolCallID: "call-1", Content: "result"},
		newToolBudgetMessage(1, 5),
	})
	require.True(t, ok)
	assert.Equal(t, format, got)
}

func runtimeStatusIntField(t *testing.T, content, name string) int {
	t.Helper()
	prefix := name + ": "
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, prefix); ok {
			n, err := strconv.Atoi(value)
			require.NoError(t, err)
			return n
		}
	}
	t.Fatalf("runtime status field %q not found in %s", name, content)
	return 0
}
