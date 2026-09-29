package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
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

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
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
					{ID: runX1, Function: agent.ToolCallFunction{Name: agent.SpawnSubagentTool,
						Arguments: `{"task":"collect data from alpha"}`}},
					{ID: runX2, Function: agent.ToolCallFunction{Name: agent.SpawnSubagentTool,
						Arguments: `{"task":"collect data from beta"}`}},
					{ID: runX3, Function: agent.ToolCallFunction{Name: agent.SpawnSubagentTool,
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
	var instanceIDs []string
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
		instanceID, _ := meta[stream.MetaAgentInstanceID].(string)
		if instanceID != "" {
			require.Contains(t, []string{runX1, runX2, runX3}, runID,
				"every sub-agent %s event must carry a valid run id, got %q", ty, runID)
			if len(instanceIDs) == 0 || instanceIDs[len(instanceIDs)-1] != instanceID {
				instanceIDs = append(instanceIDs, instanceID)
			}
			if ty == stream.EventToken {
				perRun[runID] += p["content"].(string)
				sseTokenRunOrder = append(sseTokenRunOrder, runID)
			}
		} else {
			assert.Empty(t, runID, "non-sub-agent %s event must not carry a run id", ty)
			assert.Empty(t, instanceID, "non-sub-agent %s event must not carry an instance id", ty)
		}
	}
	require.Len(t, uniqueStrings(instanceIDs), 3, "parallel instances must receive distinct stable ids")
	// The three runs' tokens interleave on the wire in the gated order.
	assert.Equal(t, []string{runX1, runX2, runX3, runX1, runX2, runX3}, sseTokenRunOrder,
		"SSE token events must interleave across runs (arrival order, attributable by id)")
	// Grouped by run id, each run's text is coherent.
	assert.Equal(t, "alpha-one alpha-two", perRun[runX1])
	assert.Equal(t, "beta-one beta-two", perRun[runX2])
	assert.Equal(t, "gamma-one gamma-two", perRun[runX3])
	requireEventPresent(t, types, stream.EventDone)

	// --- Persistence: arrival order, per-run attribution ------------------
	// Post refactor-message-storage the split is structural: sub-agent events
	// NEVER land in messages — their transcript lives in subagent_runs — and
	// messages holds only user + Confucius round finals (identity-free).
	env.waitForPersistedTurn(t, defaultSessionID, 3)
	msgs := env.mysqlFake.messagesFor(defaultSessionID)
	runRows := env.mysqlFake.snapshotSubAgentRuns(defaultSessionID)
	knownInstances := map[string]bool{}
	for _, r := range runRows {
		knownInstances[r.AgentInstanceID] = true
	}
	for _, m := range msgs {
		if m.ToolCallID != "" {
			// A spawn PLACEHOLDER links to its dispatched instance; the
			// instance must be one of this session's recorded runs.
			assert.True(t, knownInstances[m.AgentInstanceID],
				"spawn placeholder links to a recorded instance: %s", m.AgentInstanceID)
			assert.Empty(t, m.RunID, "the placeholder is a parent-level row")
			continue
		}
		assert.Empty(t, m.AgentInstanceID,
			"messages carry no sub-agent rows (got %s/%s)", m.Agent, m.EventType)
		assert.Empty(t, m.RunID, "top-level row (%s/%s) must have empty run_id", m.Agent, m.EventType)
	}
	// The three interleaved runs' transcripts are complete in subagent_runs.
	perRunSnap := map[string]string{}
	for _, r := range runRows {
		var snap []agent.Message
		require.NoError(t, json.Unmarshal(r.MessagesJSON, &snap))
		for _, sm := range snap {
			if sm.Role == "assistant" {
				perRunSnap[r.RunID] += sm.Content
			}
		}
	}
	assert.Equal(t, "alpha-one alpha-two", perRunSnap[runX1])
	assert.Equal(t, "beta-one beta-two", perRunSnap[runX2])
	assert.Equal(t, "gamma-one gamma-two", perRunSnap[runX3])

	// --- Redis read path round trip ---------------------------------------
	recovered, err := env.msgSvc.RecoverMessages(context.Background(), defaultUserID, defaultSessionID)
	require.NoError(t, err)
	for _, m := range recovered {
		// Spawn placeholders keep their instance link through the Redis read
		// path; content rows still never appear.
		if m.ToolCallID == "" {
			assert.Empty(t, m.AgentInstanceID, "Redis read path carries no sub-agent content rows")
		}
	}
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
				{ID: "call_only", Function: agent.ToolCallFunction{Name: agent.SpawnSubagentTool,
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
		instanceID, _ := meta[stream.MetaAgentInstanceID].(string)
		if instanceID != "" {
			assert.Equal(t, "call_only", runID, "the single invocation's events carry its id")
		} else {
			assert.Empty(t, runID, "top-level %s event carries no run id", ty)
			assert.Empty(t, instanceID, "top-level %s event carries no instance id", ty)
		}
	}
	requireEventPresent(t, types, stream.EventDone)

	env.waitForPersistedTurn(t, defaultSessionID, 3)
	instances := env.mysqlFake.snapshotSubAgentInstances(defaultSessionID)
	known := map[string]bool{}
	for _, inst := range instances {
		known[inst.AgentInstanceID] = true
	}
	for _, m := range env.mysqlFake.messagesFor(defaultSessionID) {
		// Sub-agent rows never enter messages; a spawn PLACEHOLDER may carry
		// the dispatched instance id as its transcript link.
		if m.ToolCallID == "call_only" {
			assert.True(t, known[m.AgentInstanceID],
				"the spawn placeholder links to the recorded instance")
			continue
		}
		assert.Empty(t, m.RunID, "row %s/%s must have no run id", m.Agent, m.EventType)
		assert.Empty(t, m.AgentInstanceID, "row %s/%s must have no instance id", m.Agent, m.EventType)
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

	env.waitForPersistedTurn(t, defaultSessionID, 2)
	for _, m := range env.mysqlFake.messagesFor(defaultSessionID) {
		assert.Empty(t, m.RunID, "row %s/%s must have no run id", m.Agent, m.EventType)
	}
}
