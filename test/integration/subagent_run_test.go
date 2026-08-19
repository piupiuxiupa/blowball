package integration

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

// turnGate serializes concurrent token emissions into a fixed global order:
// each scheduled token has a slot, and a run may emit only when the global
// turn counter reaches its slot. This gives the parallel-dispatch test a
// DETERMINISTIC interleaving (x1,x2,x3,x1,x2,x3,...) instead of scheduler
// roulette.
type turnGate struct {
	mu   sync.Mutex
	cond *sync.Cond
	turn int
}

func newTurnGate() *turnGate {
	g := &turnGate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *turnGate) waitTurn(slot int) {
	g.mu.Lock()
	for g.turn < slot {
		g.cond.Wait()
	}
	g.mu.Unlock()
}

func (g *turnGate) advance() {
	g.mu.Lock()
	g.turn++
	g.cond.Broadcast()
	g.mu.Unlock()
}

// scheduledToken is one token emission pinned to a global slot.
type scheduledToken struct {
	slot int
	text string
}

// subAgentScript is one interleaved sub-agent run's emission plan.
type subAgentScript struct {
	taskMarker string // substring of the run's task text
	tokens     []scheduledToken
	usage      agent.Usage
}

// interleavingLLMClient drives a turn with three parallel same-name
// invoke_chongzhi dispatches whose token streams interleave deterministically.
// Sub-agent calls (identified by the "chongzhi" system prompt) route to their
// run's script by task text; every other call (Confucius rounds, title
// generation) pops the embedded scripted client.
type interleavingLLMClient struct {
	inner   *scriptedLLMClient
	gate    *turnGate
	scripts []subAgentScript
}

func (c *interleavingLLMClient) scriptFor(task string) *subAgentScript {
	for i := range c.scripts {
		if strings.Contains(task, c.scripts[i].taskMarker) {
			return &c.scripts[i]
		}
	}
	return nil
}

func (c *interleavingLLMClient) StreamChat(ctx context.Context, req agent.LLMRequest, onToken func(string) error, onReasoning func(string) error) (agent.LLMResponse, error) {
	if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "chongzhi") {
		task := ""
		for _, m := range req.Messages {
			if m.Role == "user" {
				task = m.Content
				break
			}
		}
		script := c.scriptFor(task)
		if script == nil {
			panic("interleavingLLMClient: no script for sub-agent task " + task)
		}
		var streamed strings.Builder
		for _, st := range script.tokens {
			c.gate.waitTurn(st.slot)
			if err := ctx.Err(); err != nil {
				return agent.LLMResponse{}, err
			}
			if onToken != nil {
				if err := onToken(st.text); err != nil {
					return agent.LLMResponse{}, err
				}
			}
			streamed.WriteString(st.text)
			c.gate.advance()
		}
		return agent.LLMResponse{
			FinishReason: "stop",
			Content:      streamed.String(),
			Usage:        script.usage,
		}, nil
	}
	return c.inner.StreamChat(ctx, req, onToken, onReasoning)
}

// TestSubAgentRun_IdentityEndToEnd (task 5.1): one turn dispatches three
// parallel invoke_chongzhi calls (x1/x2/x3) whose token streams interleave
// deterministically. Verifies the capability end-to-end:
//   - SSE: every sub-agent event carries meta.parent_tool_call_id, and the
//     token events interleave on the wire yet regroup into three coherent
//     per-run outputs; Confucius events carry no run identity.
//   - Persistence: rows land in arrival order with per-run attribution
//     (MergeEvents treats a run-id change as a boundary, so interleaved
//     fragments never merge across runs), and the Redis→MySQL round trip
//     (write-behind flush + RecoverMessages) preserves run_id.
func TestSubAgentRun_IdentityEndToEnd(t *testing.T) {
	const (
		runX1 = "call_x1"
		runX2 = "call_x2"
		runX3 = "call_x3"
	)
	// Global emission order: x1a, x2a, x3a, x1b, x2b, x3b.
	gate := newTurnGate()
	llm := &interleavingLLMClient{
		inner: newScriptedLLMClient(
			// Confucius round 1: three parallel same-name invokes.
			scriptedLLMResponse{
				finishReason: "tool_calls",
				toolCalls: []agent.ToolCall{
					{ID: runX1, Function: agent.ToolCallFunction{Name: agent.ToolInvokeChongzhi,
						Arguments: `{"task":"collect data from alpha"}`}},
					{ID: runX2, Function: agent.ToolCallFunction{Name: agent.ToolInvokeChongzhi,
						Arguments: `{"task":"collect data from beta"}`}},
					{ID: runX3, Function: agent.ToolCallFunction{Name: agent.ToolInvokeChongzhi,
						Arguments: `{"task":"collect data from gamma"}`}},
				},
				usage: agent.Usage{PromptTokens: 30, CompletionTokens: 3, TotalTokens: 33},
			},
			// Confucius round 2: final summary.
			scriptedLLMResponse{
				tokens:       []string{"all ", "three ", "done"},
				content:      "all three done",
				finishReason: "stop",
				usage:        agent.Usage{PromptTokens: 50, CompletionTokens: 3, TotalTokens: 53},
			},
			// Async title generation (fires after the turn).
			scriptedLLMResponse{
				content:      "Parallel collection",
				finishReason: "stop",
				usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			},
		),
		gate: gate,
		scripts: []subAgentScript{
			{taskMarker: "alpha", usage: agent.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
				tokens: []scheduledToken{{slot: 0, text: "alpha-one "}, {slot: 3, text: "alpha-two"}}},
			{taskMarker: "beta", usage: agent.Usage{PromptTokens: 11, CompletionTokens: 2, TotalTokens: 13},
				tokens: []scheduledToken{{slot: 1, text: "beta-one "}, {slot: 4, text: "beta-two"}}},
			{taskMarker: "gamma", usage: agent.Usage{PromptTokens: 12, CompletionTokens: 2, TotalTokens: 14},
				tokens: []scheduledToken{{slot: 2, text: "gamma-one "}, {slot: 5, text: "gamma-two"}}},
		},
	}

	env := newTestEnv(t, llm)
	token := authToken(t, defaultUserID)
	w := env.postMessage(`{"content":"collect from all three sources"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	types, payloads := parseSSEBody(t, w.Body.String())
	require.NotEmpty(t, types)

	// --- SSE attribution -------------------------------------------------
	perRun := map[string]string{runX1: "", runX2: "", runX3: ""}
	var sseTokenRunOrder []string
	for i, ty := range types {
		p := payloads[i]
		meta, _ := p["meta"].(map[string]any)
		runID := ""
		if meta != nil {
			if s, ok := meta[stream.MetaParentToolCallID].(string); ok {
				runID = s
			}
		}
		if p["agent"] == stream.AgentChongzhi {
			require.Contains(t, []string{runX1, runX2, runX3}, runID,
				"every sub-agent %s event must carry a valid run id, got %q", ty, runID)
			if ty == stream.EventToken {
				perRun[runID] += p["content"].(string)
				sseTokenRunOrder = append(sseTokenRunOrder, runID)
			}
		} else {
			assert.Empty(t, runID, "non-sub-agent %s event must not carry a run id", ty)
		}
	}
	// The three runs' tokens interleave on the wire in the gated order.
	assert.Equal(t, []string{runX1, runX2, runX3, runX1, runX2, runX3}, sseTokenRunOrder,
		"SSE token events must interleave across runs (arrival order, attributable by id)")
	// Grouped by run id, each run's text is coherent.
	assert.Equal(t, "alpha-one alpha-two", perRun[runX1])
	assert.Equal(t, "beta-one beta-two", perRun[runX2])
	assert.Equal(t, "gamma-one gamma-two", perRun[runX3])
	requireEventPresent(t, types, stream.EventDone)

	// --- Persistence: arrival order, per-run attribution ------------------
	env.waitForPersistedTurn(t, defaultSessionID, 20)
	msgs := env.mysqlFake.messagesFor(defaultSessionID)

	var rowRunOrder []string
	perRunRows := map[string]string{runX1: "", runX2: "", runX3: ""}
	for _, m := range msgs {
		if m.Agent != stream.AgentChongzhi {
			// Top-level rows (user, Confucius) never carry a run id.
			assert.Empty(t, m.RunID, "non-sub-agent row (%s/%s) must have empty run_id", m.Agent, m.EventType)
			continue
		}
		if m.EventType != model.EventTypeToken {
			assert.Contains(t, []string{runX1, runX2, runX3}, m.RunID,
				"every sub-agent row must carry its run id (%s)", m.EventType)
			continue
		}
		require.Contains(t, []string{runX1, runX2, runX3}, m.RunID)
		rowRunOrder = append(rowRunOrder, m.RunID)
		perRunRows[m.RunID] += m.Content
	}
	// Interleaved arrival persists as separate per-run rows (MergeEvents broke
	// on every run-id change) — the pre-change behavior glued them into one.
	assert.Equal(t, []string{runX1, runX2, runX3, runX1, runX2, runX3}, rowRunOrder,
		"token rows must persist interleaved, each attributed to its run")
	assert.Equal(t, "alpha-one alpha-two", perRunRows[runX1])
	assert.Equal(t, "beta-one beta-two", perRunRows[runX2])
	assert.Equal(t, "gamma-one gamma-two", perRunRows[runX3])

	// --- Redis read path round trip ---------------------------------------
	recovered, err := env.msgSvc.RecoverMessages(context.Background(), defaultUserID, defaultSessionID)
	require.NoError(t, err)
	recoveredRuns := map[string]string{}
	for _, m := range recovered {
		if m.Agent == stream.AgentChongzhi && m.EventType == model.EventTypeToken {
			recoveredRuns[m.RunID] += m.Content
		}
	}
	assert.Equal(t, "alpha-one alpha-two", recoveredRuns[runX1], "run_id must survive the Redis read path")
	assert.Equal(t, "beta-one beta-two", recoveredRuns[runX2])
	assert.Equal(t, "gamma-one gamma-two", recoveredRuns[runX3])
}

// TestSubAgentRun_SingleInvocationRegression (task 5.2): a turn with ONE
// sub-agent dispatch behaves exactly as before the capability — the sub-agent
// events all carry the single invocation id, top-level events and rows carry
// none, and the turn completes normally.
func TestSubAgentRun_SingleInvocationRegression(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{
				{ID: "call_only", Function: agent.ToolCallFunction{Name: agent.ToolInvokeChongzhi,
					Arguments: `{"task":"write hello"}`}},
			},
			usage: agent.Usage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22},
		},
		scriptedLLMResponse{
			tokens:       []string{"sub ", "result"},
			content:      "sub result",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
		},
		scriptedLLMResponse{
			tokens:       []string{"merged ", "answer"},
			content:      "merged answer",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 42},
		},
		scriptedLLMResponse{content: "Single", finishReason: "stop"},
	)

	env := newTestEnv(t, llm)
	token := authToken(t, defaultUserID)
	w := env.postMessage(`{"content":"single dispatch please"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	types, payloads := parseSSEBody(t, w.Body.String())
	for i, ty := range types {
		p := payloads[i]
		meta, _ := p["meta"].(map[string]any)
		runID := ""
		if meta != nil {
			if s, ok := meta[stream.MetaParentToolCallID].(string); ok {
				runID = s
			}
		}
		if p["agent"] == stream.AgentChongzhi {
			assert.Equal(t, "call_only", runID, "the single invocation's events carry its id")
		} else {
			assert.Empty(t, runID, "top-level %s event carries no run id", ty)
		}
	}
	requireEventPresent(t, types, stream.EventDone)

	env.waitForPersistedTurn(t, defaultSessionID, 5)
	for _, m := range env.mysqlFake.messagesFor(defaultSessionID) {
		if m.Agent == stream.AgentChongzhi {
			assert.Equal(t, "call_only", m.RunID)
		} else {
			assert.Empty(t, m.RunID, "row %s/%s must have no run id", m.Agent, m.EventType)
		}
	}
}

// TestSubAgentRun_NoSubAgentRegression (task 5.2): a plain Confucius-only
// turn is byte-for-byte the pre-change shape — no event carries a run id and
// no row persists one.
func TestSubAgentRun_NoSubAgentRegression(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"direct ", "answer"},
			content:      "direct answer",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10},
		},
		scriptedLLMResponse{content: "Plain", finishReason: "stop"},
	)

	env := newTestEnv(t, llm)
	token := authToken(t, defaultUserID)
	w := env.postMessage(`{"content":"just answer"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	types, payloads := parseSSEBody(t, w.Body.String())
	require.NotEmpty(t, types)
	for i, ty := range types {
		meta, _ := payloads[i]["meta"].(map[string]any)
		if meta == nil {
			continue
		}
		_, has := meta[stream.MetaParentToolCallID]
		assert.False(t, has, "a no-sub-agent turn must not emit %s events with a run id", ty)
	}
	requireEventPresent(t, types, stream.EventDone)

	env.waitForPersistedTurn(t, defaultSessionID, 3)
	for _, m := range env.mysqlFake.messagesFor(defaultSessionID) {
		assert.Empty(t, m.RunID, "row %s/%s must have no run id", m.Agent, m.EventType)
	}
}
