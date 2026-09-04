package agent

import (
	"context"
	"testing"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool/skill"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// newTurnConfigOrchestrator builds an orchestrator whose three agents carry
// distinct prompts but NO model or quota fields — model/effort/quota are
// turn-level only (model-effort-v2, per-model-completion-budget) — so any
// turn-config leakage (or absence) is observable in the recorded LLM requests.
func newTurnConfigOrchestrator(t *testing.T, client LLMClient) *Orchestrator {
	t.Helper()
	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{APIKey: "test"},
		Agents: config.AgentsConfig{
			Confucius: config.AgentConfig{
				Name: "Confucius", SystemPrompt: "you are confucius",
			},
			Subagent: config.SubAgentConfig{
				AgentConfig: config.AgentConfig{
					Name: "Chongzhi", SystemPrompt: "you are chongzhi",
				},
				MaxDepth:         1,
				MaxConcurrent:    4,
				MaxTotalPerTurn:  12,
				MaxSnapshotBytes: 1 << 20,
			},
		},
	}
	o, err := NewOrchestrator(client, cfg, nil, nil, skill.NewLoader("", nil), nil)
	require.NoError(t, err)
	return o
}

// dispatchScript is the 4-response script that drives Confucius to dispatch
// BOTH sub-agents in one round and then finish: Confucius tool_call round →
// Chongzhi's single call → Liang's single call → Confucius's final round.
func dispatchScript() []fakeResponse {
	return []fakeResponse{
		{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{
				{ID: "c1", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: `{"task":"t1","name":"Chongzhi"}`}},
				{ID: "c2", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: `{"task":"t2","name":"Liang"}`}},
			},
			usage: Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
		},
		{content: "chongzhi-done", tokens: []string{"chongzhi-done"}, finishReason: "stop", usage: Usage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22}},
		{content: "liang-done", tokens: []string{"liang-done"}, finishReason: "stop", usage: Usage{PromptTokens: 30, CompletionTokens: 3, TotalTokens: 33}},
		{content: "final", tokens: []string{"final"}, finishReason: "stop", usage: Usage{PromptTokens: 40, CompletionTokens: 4, TotalTokens: 44}},
	}
}

// callSignature is the model/thinking projection of one recorded LLMRequest.
type callSignature struct {
	model     string
	think     bool
	effort    string
	maxTokens int
}

// signatures snapshots the fake client's recorded requests as signatures.
func signatures(f *fakeLLMClient) []callSignature {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]callSignature, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, callSignature{model: c.Model, think: c.Thinking, effort: c.ReasoningEffort, maxTokens: c.MaxCompletionTokens})
	}
	return out
}

// runTurn drives one orchestrator turn to completion.
func runTurn(t *testing.T, o *Orchestrator, override ModelOverride) {
	t.Helper()
	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// Consume the hub like WriteSSE does: drain events, stop on Close's done
	// signal with a final drain (Close closes done, not the events channel).
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			select {
			case <-hub.Events():
			default:
				select {
				case <-hub.Events():
				case <-hub.Done():
				drain:
					for {
						select {
						case <-hub.Events():
						default:
							break drain
						}
					}
					return
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	err := o.Handle(ctx, t.TempDir(), t.TempDir(), "user-1", []Message{{Role: "user", Content: "hi"}}, hub, nil, override)
	hub.Close()
	<-consumerDone
	require.NoError(t, err)
}

// TestOrchestrator_TurnConfig_ReachesAllAgents verifies the handler-resolved
// turn config reaches EVERY LLM call of the turn — Confucius's two rounds and
// both dispatched sub-agents — including the resolved entry's output quota,
// which all agents of the turn share (per-model-completion-budget: agents
// carry no quota field; the ModelOverride is their only quota source).
func TestOrchestrator_TurnConfig_ReachesAllAgents(t *testing.T) {
	defer goleak.VerifyNone(t)

	client := newFake(dispatchScript()...)
	o := newTurnConfigOrchestrator(t, client)
	runTurn(t, o, ModelOverride{Model: "sel-x", Thinking: true, ReasoningEffort: "xhigh", MaxCompletionTokens: 1024})

	sigs := signatures(client)
	require.Len(t, sigs, 4, "want Confucius×2 + Chongzhi + Liang LLM calls")
	for i, s := range sigs {
		assert.Equal(t, "sel-x", s.model, "call %d model", i)
		assert.True(t, s.think, "call %d wire family", i)
		assert.Equal(t, "xhigh", s.effort, "call %d effort", i)
		assert.Equal(t, 1024, s.maxTokens, "call %d quota (turn-resolved entry)", i)
	}
}

// TestOrchestrator_TurnConfig_NoneEffortStaysLiteral verifies the B2 wire
// family: a thinking entry with effort "none" keeps Thinking=true and the
// literal "none" on every recorded call — the client sends the parameter
// instead of omitting it (the glm "thinking stays on unless none is
// explicit" fix).
func TestOrchestrator_TurnConfig_NoneEffortStaysLiteral(t *testing.T) {
	defer goleak.VerifyNone(t)

	client := newFake(dispatchScript()...)
	o := newTurnConfigOrchestrator(t, client)
	runTurn(t, o, ModelOverride{Model: "glm-5.2", Thinking: true, ReasoningEffort: "none"})

	sigs := signatures(client)
	require.Len(t, sigs, 4)
	for i, s := range sigs {
		assert.True(t, s.think, "call %d wire family stays thinking", i)
		assert.Equal(t, "none", s.effort, "call %d effort must stay the literal none", i)
	}
}

// TestOrchestrator_TurnConfig_NonThinkingFamilyPropagates: a thinking:false
// entry propagates the non-thinking wire family — every recorded call carries
// Thinking=false (the client then omits reasoning_effort entirely).
func TestOrchestrator_TurnConfig_NonThinkingFamilyPropagates(t *testing.T) {
	defer goleak.VerifyNone(t)

	client := newFake(dispatchScript()...)
	o := newTurnConfigOrchestrator(t, client)
	runTurn(t, o, ModelOverride{Model: "gpt-4o", Thinking: false, ReasoningEffort: "none"})

	sigs := signatures(client)
	require.Len(t, sigs, 4)
	for i := range sigs {
		assert.False(t, sigs[i].think, "call %d wire family", i)
	}
}
