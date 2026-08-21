package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// pingTool registers a single no-op "ping" tool so tooled agents can dispatch
// tool_calls in round-cap tests.
func pingTool(t *testing.T) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry()
	require.NoError(t, reg.Register(&tool.ToolSpec{
		Name:           "ping",
		Description:    "reply with pong",
		ParametersJSON: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			return "pong", nil
		},
	}))
	return reg
}

// toolCallResp builds a fakeResponse that emits one "ping" tool_call
// (finish_reason tool_calls), driving the agent loop another dispatch round.
func toolCallResp(id string) fakeResponse {
	return fakeResponse{
		tokens:       []string{"call"},
		content:      "",
		finishReason: "tool_calls",
		toolCalls: []ToolCall{{ID: id, Function: ToolCallFunction{
			Name: "ping", Arguments: `{"msg":"x"}`,
		}}},
		usage: Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}
}

// drainBufferedEvents pulls every currently-buffered event out of hub. It is
// meant to be called AFTER Run has returned synchronously and hub.Close() has
// been called: by then every SendCtx has completed, so all events are buffered
// and a default-select drains them without blocking.
func drainBufferedEvents(hub *stream.Hub) []stream.StreamEvent {
	var out []stream.StreamEvent
	for {
		select {
		case e := <-hub.Events():
			out = append(out, e)
		default:
			return out
		}
	}
}

// hasAgentError reports whether events contain an agent_error with the given
// error_code (Meta[MetaCode]).
func hasAgentError(events []stream.StreamEvent, code string) bool {
	for _, e := range events {
		if e.Type != stream.EventAgentError {
			continue
		}
		if c, _ := e.Meta[stream.MetaCode].(string); c == code {
			return true
		}
	}
	return false
}

// TestChongzhi_RoundCap_SuccessfulWrapUp: exhausting max_rounds runs one
// tool-disabled wrap-up round; its content becomes the answer, LastRunHitCap is
// true, the wrap-up request omits tools, and the wrap-up usage folds into the
// run total (tasks 7.2a, 7.5, 7.6).
func TestChongzhi_RoundCap_SuccessfulWrapUp(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := pingTool(t)
	// 2 tool_call rounds exhaust the cap (max_rounds=2); the 3rd response is
	// the tool-disabled wrap-up round.
	client := newFake(
		toolCallResp("tc_1"),
		toolCallResp("tc_2"),
		fakeResponse{
			tokens:       []string{"S", "U", "M"},
			content:      "SUMMARY",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 3, CompletionTokens: 3, TotalTokens: 6},
		},
	)
	c, err := NewChongzhi(config.AgentConfig{
		Name: "Chongzhi", SystemPrompt: "you code",
		Tools: []string{"ping"}, MaxRounds: 2,
	}, client, reg, testTurn())
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	content, usage, _, runErr := c.Run(ctx, []Message{{Role: "user", Content: "do it"}}, hub)
	hub.Close()

	require.NoError(t, runErr, "successful wrap-up returns no error")
	assert.Equal(t, "SUMMARY", content)
	assert.True(t, c.LastRunHitCap(), "LastRunHitCap must be true after a cap exit")
	// Task 7.5: the wrap-up round (last request) must carry no tools.
	last := client.lastRequest()
	assert.Nil(t, last.Tools, "wrap-up request must omit tools")
	// Task 7.6: wrap-up usage (6) folded with the two dispatch rounds (2 each).
	assert.Equal(t, 10, usage.TotalTokens, "wrap-up usage must be folded into the run total")
	// 3 LLM calls: 2 dispatch rounds + 1 wrap-up.
	assert.Equal(t, 3, client.requestCount())
}

// TestChongzhi_RoundCap_EmptyWrapUp_SurfacesError: when the wrap-up round
// yields no content, the turn surfaces the loud signal — agent_error
// (round_cap_exhausted) is emitted and Run returns a non-nil error. The WARN
// log fires in the same cap block (covered by this code path) (task 7.2b/7.2c).
func TestChongzhi_RoundCap_EmptyWrapUp_SurfacesError(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := pingTool(t)
	client := newFake(
		toolCallResp("tc_1"),
		toolCallResp("tc_2"),
		fakeResponse{content: "", finishReason: "stop"}, // empty wrap-up
	)
	c, err := NewChongzhi(config.AgentConfig{
		Name: "Chongzhi", SystemPrompt: "you code",
		Tools: []string{"ping"}, MaxRounds: 2,
	}, client, reg, testTurn())
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _, runErr := c.Run(ctx, []Message{{Role: "user", Content: "do it"}}, hub)
	hub.Close()

	require.Error(t, runErr, "empty wrap-up must return a non-nil error")
	assert.True(t, c.LastRunHitCap())
	assert.True(t, hasAgentError(drainBufferedEvents(hub), "round_cap_exhausted"),
		"must emit agent_error round_cap_exhausted on empty wrap-up")
}

// TestChongzhi_NaturalStop_NoCapSignal: a natural stop before the cap leaves
// LastRunHitCap false and emits no agent_error (task 7.2d).
func TestChongzhi_NaturalStop_NoCapSignal(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := pingTool(t)
	client := newFake(
		toolCallResp("tc_1"),
		fakeResponse{tokens: []string{"done"}, content: "done", finishReason: "stop"},
	)
	c, err := NewChongzhi(config.AgentConfig{
		Name: "Chongzhi", SystemPrompt: "you code",
		Tools: []string{"ping"}, MaxRounds: 5, // generous cap, not hit
	}, client, reg, testTurn())
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	content, _, _, runErr := c.Run(ctx, []Message{{Role: "user", Content: "do it"}}, hub)
	hub.Close()

	require.NoError(t, runErr)
	assert.Equal(t, "done", content)
	assert.False(t, c.LastRunHitCap(), "natural stop must not set LastRunHitCap")
	for _, e := range drainBufferedEvents(hub) {
		if e.Type == stream.EventAgentError {
			t.Fatalf("natural stop must not emit any agent_error; got %+v", e)
		}
	}
}

// TestChongzhi_RoundCap_WrapUpReturnsToolCalls_SurfacesError: when the wrap-up
// round emits tool_calls (some OpenAI-compatible gateways do even with tools
// stripped from the request), those calls cannot be honored and the prose is a
// preamble, not a synthesized answer. The turn must surface the loud
// round_cap_exhausted signal rather than silently returning the preamble as the
// final answer (the bug observed in production with glm-5.2 at max_rounds=2).
func TestChongzhi_RoundCap_WrapUpReturnsToolCalls_SurfacesError(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := pingTool(t)
	client := newFake(
		toolCallResp("tc_1"),
		toolCallResp("tc_2"),
		// Wrap-up round: the model ignores the disabled-tools instruction and
		// emits a tool_call plus a "I'll do X next" preamble.
		fakeResponse{
			tokens:       []string{"next"},
			content:      "我需要用 mcp_list_tools 来获取工具列表。",
			finishReason: "tool_calls",
			toolCalls: []ToolCall{{ID: "tc_3", Function: ToolCallFunction{
				Name: "ping", Arguments: `{}`,
			}}},
			usage: Usage{TotalTokens: 5},
		},
	)
	c, err := NewChongzhi(config.AgentConfig{
		Name: "Chongzhi", SystemPrompt: "you code",
		Tools: []string{"ping"}, MaxRounds: 2,
	}, client, reg, testTurn())
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	content, _, _, runErr := c.Run(ctx, []Message{{Role: "user", Content: "do it"}}, hub)
	hub.Close()

	require.Error(t, runErr, "wrap-up tool_calls must surface as an error, not a silent preamble")
	assert.True(t, c.LastRunHitCap())
	assert.True(t, hasAgentError(drainBufferedEvents(hub), "round_cap_exhausted"),
		"must emit agent_error round_cap_exhausted when the wrap-up emits tool_calls")
	// The preamble must NOT be returned as the final content.
	assert.Equal(t, "", content, "preamble from a tool_calling wrap-up must not be the final answer")
}

// TestConfucius_RoundCap_SuccessfulWrapUp: Confucius hitting its own cap runs a
// wrap-up round, sets LastRunHitCap, marks the turn RoundCapped, and omits
// tools on the wrap-up request (tasks 7.2a, 7.4, 7.5).
func TestConfucius_RoundCap_SuccessfulWrapUp(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := pingTool(t)
	subAgents := staticFactories(map[string]Agent{
		ToolInvokeChongzhi: &fakeAgent{name: "Chongzhi"},
		ToolInvokeLiang:    &fakeAgent{name: "Liang"},
	})
	client := newFake(
		toolCallResp("tc_1"),
		toolCallResp("tc_2"),
		fakeResponse{tokens: []string{"wrap"}, content: "WRAPUP", finishReason: "stop",
			usage: Usage{TotalTokens: 5}},
	)
	c, err := NewConfucius(config.AgentConfig{
		Name: "Confucius", SystemPrompt: "you orchestrate",
		Tools: []string{"ping"}, MaxRounds: 2,
	}, client, reg, subAgents, testTurn())
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	content, _, bd, runErr := c.Run(ctx, []Message{{Role: "user", Content: "do"}}, hub)
	hub.Close()

	require.NoError(t, runErr)
	assert.Equal(t, "WRAPUP", content)
	assert.True(t, c.LastRunHitCap())
	require.NotNil(t, bd)
	assert.True(t, bd.RoundCapped, "Confucius hitting its own cap sets turn RoundCapped")
	assert.Nil(t, client.lastRequest().Tools, "wrap-up request must omit tools")
	assert.Equal(t, true, buildMetaObject(bd)["round_capped"])
}

// TestLiang_RoundCap_WrapUpAppliesResponseFormat: a structured-output Liang
// hitting the cap carries response_format on the tool-disabled wrap-up round
// (the wrap-up IS the terminal round) (task 7.3).
func TestLiang_RoundCap_WrapUpAppliesResponseFormat(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := pingTool(t)
	client := newFake(
		toolCallResp("tc_1"),
		toolCallResp("tc_2"),
		fakeResponse{tokens: []string{"{}"}, content: `{"answer":"ok"}`, finishReason: "stop"},
	)
	liang, err := NewLiang(config.AgentConfig{
		Name: "Liang", SystemPrompt: "you analyze",
		Tools: []string{"ping"}, MaxRounds: 2,
		OutputSchema: `{"type":"object","properties":{"answer":{"type":"string"}}}`,
	}, client, reg, testTurn())
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _, runErr := liang.Run(ctx, []Message{{Role: "user", Content: "analyze"}}, hub)
	hub.Close()
	require.NoError(t, runErr)

	last := client.lastRequest()
	require.NotEmpty(t, last.ResponseFormat, "wrap-up round must carry response_format for structured Liang")
	assert.Nil(t, last.Tools, "wrap-up round must omit tools")
	assert.True(t, liang.LastRunHitCap())
}

// TestConfucius_SubAgentCapPropagatesToMeta: a sub-agent reporting LastRunHitCap
// marks the turn-level RoundCapped and buildMetaObject emits round_capped=true
// (task 7.4, positive case).
func TestConfucius_SubAgentCapPropagatesToMeta(t *testing.T) {
	defer goleak.VerifyNone(t)
	chongzhi := &fakeAgent{name: "Chongzhi", content: "did stuff", hitCap: true}
	liang := &fakeAgent{name: "Liang", content: "analyzed"}
	subAgents := staticFactories(map[string]Agent{
		ToolInvokeChongzhi: chongzhi,
		ToolInvokeLiang:    liang,
	})
	client := newFake(
		// Round 1: dispatch Chongzhi (which reports it hit its cap).
		fakeResponse{finishReason: "tool_calls", toolCalls: []ToolCall{{ID: "tc_1",
			Function: ToolCallFunction{Name: "invoke_chongzhi", Arguments: `{"task":"code"}`}}},
			usage: Usage{TotalTokens: 3}},
		// Round 2: natural stop (Confucius itself does not hit its cap).
		fakeResponse{tokens: []string{"ok"}, content: "ok", finishReason: "stop", usage: Usage{TotalTokens: 2}},
	)
	c, err := NewConfucius(config.AgentConfig{
		Name: "Confucius", SystemPrompt: "you orchestrate",
		MaxRounds: 10,
	}, client, tool.NewRegistry(), subAgents, testTurn())
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, bd, runErr := c.Run(ctx, []Message{{Role: "user", Content: "do"}}, hub)
	hub.Close()
	require.NoError(t, runErr)
	require.NotNil(t, bd)
	assert.True(t, bd.RoundCapped, "a capped sub-agent must mark the turn RoundCapped")
	assert.Equal(t, true, buildMetaObject(bd)["round_capped"])
}

// TestConfucius_NoCap_OmitsRoundCappedMeta: a turn where no agent hit its cap
// omits the round_capped key entirely (additive, identical to pre-change
// serialization) (task 7.4, negative case).
func TestConfucius_NoCap_OmitsRoundCappedMeta(t *testing.T) {
	defer goleak.VerifyNone(t)
	subAgents := staticFactories(map[string]Agent{
		ToolInvokeChongzhi: &fakeAgent{name: "Chongzhi", content: "did stuff"},
		ToolInvokeLiang:    &fakeAgent{name: "Liang", content: "analyzed"},
	})
	client := newFake(
		fakeResponse{finishReason: "tool_calls", toolCalls: []ToolCall{{ID: "tc_1",
			Function: ToolCallFunction{Name: "invoke_chongzhi", Arguments: `{"task":"code"}`}}}},
		fakeResponse{tokens: []string{"ok"}, content: "ok", finishReason: "stop"},
	)
	c, err := NewConfucius(config.AgentConfig{
		Name: "Confucius", SystemPrompt: "you orchestrate",
		MaxRounds: 10,
	}, client, tool.NewRegistry(), subAgents, testTurn())
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, bd, runErr := c.Run(ctx, []Message{{Role: "user", Content: "do"}}, hub)
	hub.Close()
	require.NoError(t, runErr)
	require.NotNil(t, bd)
	assert.False(t, bd.RoundCapped)
	_, present := buildMetaObject(bd)["round_capped"]
	assert.False(t, present, "round_capped key must be omitted when no agent hit its cap")
}
