package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

type planRoutingLLM struct {
	root    *scriptedLLMClient
	success *scriptedLLMClient
	failure *scriptedLLMClient
}

func (c *planRoutingLLM) StreamChat(ctx context.Context, req agent.LLMRequest, onToken func(string) error, onReasoning func(string) error) (agent.LLMResponse, error) {
	task := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			task = req.Messages[i].Content
			break
		}
	}
	switch {
	case strings.Contains(task, "succeed at"):
		return c.success.StreamChat(ctx, req, onToken, onReasoning)
	case strings.Contains(task, "fail loudly"):
		return c.failure.StreamChat(ctx, req, onToken, onReasoning)
	default:
		return c.root.StreamChat(ctx, req, onToken, onReasoning)
	}
}

func dynamicAgentsForTest(mutate func(*config.SubAgentConfig)) config.AgentsConfig {
	agents := agentConfig()
	if mutate != nil {
		mutate(&agents.Subagent)
	}
	return agents
}

func toolNamesFromJSON(t *testing.T, raw []byte) []string {
	t.Helper()
	var tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	require.NoError(t, json.Unmarshal(raw, &tools))
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Function.Name)
	}
	return out
}

func TestDynamicSubagents_ToolNarrowingAndDefaultDepth(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "narrow_call",
				Function: agent.ToolCallFunction{
					Name:      agent.SpawnSubagentTool,
					Arguments: `{"task":"read only","tools":["xizhi_read_file"],"name":"Reader"}`,
				},
			}},
		},
		scriptedLLMResponse{content: "read result", finishReason: "stop"},
		scriptedLLMResponse{content: "merged", finishReason: "stop"},
	)
	env := newTestEnv(t, llm)
	w := env.postMessage(`{"content":"narrow tools"}`, authToken(t, defaultUserID))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	llm.mu.Lock()
	var subTools []byte
	for _, call := range llm.calls {
		if len(call.Messages) > 1 && strings.Contains(call.Messages[1].Content, "read only") {
			subTools = call.Tools
			break
		}
	}
	llm.mu.Unlock()
	require.NotEmpty(t, subTools, "the narrowed sub-agent LLM call must be recorded")
	names := toolNamesFromJSON(t, subTools)
	assert.Equal(t, []string{"xizhi_read_file"}, names,
		"a child at default depth gets only the narrowed tool and no spawn tool")
}

func TestDynamicSubagents_ResumeEndToEnd(t *testing.T) {
	llm := newScriptedLLMClient(
		// Initial dispatch.
		scriptedLLMResponse{
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "initial_call",
				Function: agent.ToolCallFunction{
					Name:      agent.SpawnSubagentTool,
					Arguments: `{"task":"start work","name":"Worker"}`,
				},
			}},
		},
		// MaxRounds=1: this tool-calling round consumes the cap.
		scriptedLLMResponse{
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "child_call",
				Function: agent.ToolCallFunction{
					Name: "unknown_tool", Arguments: `{}`,
				},
			}},
		},
		// Tool-disabled wrap-up succeeds, so the spawn result is capped/resumable.
		scriptedLLMResponse{content: "partial result", finishReason: "stop"},
		scriptedLLMResponse{content: "first summary", finishReason: "stop"},

		// Second turn: resume the same instance.
		scriptedLLMResponse{
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "resume_call",
				Function: agent.ToolCallFunction{
					Name:      agent.SpawnSubagentTool,
					Arguments: `{"task":"continue work","resume_agent_id":"PLACEHOLDER"}`,
				},
			}},
		},
		scriptedLLMResponse{content: "continued result", finishReason: "stop"},
		scriptedLLMResponse{content: "second summary", finishReason: "stop"},
	)
	agents := dynamicAgentsForTest(func(sub *config.SubAgentConfig) {
		sub.MaxRounds = 1
	})
	env := newTestEnvWithAgents(t, llm, agents)
	token := authToken(t, defaultUserID)

	w1 := env.postMessage(`{"content":"start"}`, token)
	require.Equal(t, http.StatusOK, w1.Code, "body: %s", w1.Body.String())
	env.waitForPersistedTurn(t, defaultSessionID, 5)

	var agentID string
	for _, msg := range env.mysqlFake.messagesFor(defaultSessionID) {
		if msg.Agent == stream.AgentConfucius && msg.EventType == model.EventTypeToolResult &&
			strings.Contains(msg.Content, "status: capped") {
			m := regexp.MustCompile(`agent_id: (w-[a-z0-9]+)`).FindStringSubmatch(msg.Content)
			require.NotNil(t, m, "capped result must expose agent_id in %q", msg.Content)
			agentID = m[1]
		}
	}
	require.NotEmpty(t, agentID, "capped result must be persisted")

	// Patch the queued resume arguments now that the stable id is known.
	llm.mu.Lock()
	for i := range llm.responses {
		for j := range llm.responses[i].toolCalls {
			llm.responses[i].toolCalls[j].Function.Arguments = strings.ReplaceAll(
				llm.responses[i].toolCalls[j].Function.Arguments, "PLACEHOLDER", agentID)
		}
	}
	llm.mu.Unlock()

	w2 := env.postMessage(`{"content":"continue"}`, token)
	require.Equal(t, http.StatusOK, w2.Code, "body: %s", w2.Body.String())
	llm.mu.Lock()
	var resumeMessages []agent.Message
	for _, call := range llm.calls {
		if len(call.Messages) > 1 && strings.Contains(call.Messages[len(call.Messages)-1].Content, "continue work") {
			resumeMessages = call.Messages
			break
		}
	}
	llm.mu.Unlock()
	require.NotEmpty(t, resumeMessages, "the resumed sub-agent LLM call must be recorded")
	require.GreaterOrEqual(t, len(resumeMessages), 3, "system + persisted history + new follow-up")
	assert.Equal(t, "system", resumeMessages[0].Role)
	assert.Equal(t, "start work", resumeMessages[1].Content)
	assert.Equal(t, "continue work", resumeMessages[len(resumeMessages)-1].Content)

	run, ok, err := env.mysqlFake.GetSubAgentRun(nil, defaultSessionID, agentID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, model.SubAgentStatusCompleted, run.Status)
	assert.Contains(t, string(run.MessagesJSON), "continue work")
}

func TestAgentPlanState_EndToEnd(t *testing.T) {
	router := &planRoutingLLM{
		root: newScriptedLLMClient(
			// Turn 1: create a parallel plan, dispatch two children, then update
			// semantic state after observing their mixed execution outcomes.
			scriptedLLMResponse{
				finishReason: "tool_calls",
				toolCalls: []agent.ToolCall{
					{ID: "plan-create", Function: agent.ToolCallFunction{
						Name:      agent.UpdatePlanTool,
						Arguments: `{"steps":[{"step":"succeed at research","status":"in_progress"},{"step":"recover failed analysis","status":"in_progress"}],"explanation":"dispatch parallel work"}`,
					}},
					{ID: "spawn-good", Function: agent.ToolCallFunction{
						Name:      agent.SpawnSubagentTool,
						Arguments: `{"task":"succeed at research","name":"Researcher"}`,
					}},
					{ID: "spawn-bad", Function: agent.ToolCallFunction{
						Name:      agent.SpawnSubagentTool,
						Arguments: `{"task":"fail loudly during analysis","name":"Analyst"}`,
					}},
				},
			},
			scriptedLLMResponse{
				finishReason: "tool_calls",
				toolCalls: []agent.ToolCall{{ID: "plan-verify", Function: agent.ToolCallFunction{
					Name:      agent.UpdatePlanTool,
					Arguments: `{"steps":[{"step":"succeed at research","status":"completed"},{"step":"recover failed analysis","status":"in_progress"}],"explanation":"verified research; analysis needs recovery"}`,
				}}},
			},
			scriptedLLMResponse{content: "first turn complete", finishReason: "stop"},

			// Turn 2: a fresh ledger starts at revision 1 again.
			scriptedLLMResponse{
				finishReason: "tool_calls",
				toolCalls: []agent.ToolCall{{ID: "plan-new-turn", Function: agent.ToolCallFunction{
					Name:      agent.UpdatePlanTool,
					Arguments: `{"steps":[{"step":"continue recovery","status":"in_progress"}]}`,
				}}},
			},
			scriptedLLMResponse{content: "second turn complete", finishReason: "stop"},
		),
		success: newScriptedLLMClient(scriptedLLMResponse{
			content: "research result", tokens: []string{"research"}, finishReason: "stop",
		}),
		failure: newScriptedLLMClient(scriptedLLMResponse{
			err: errors.New("analysis transport exploded"), finishReason: "stop",
		}),
	}
	env := newTestEnv(t, router)
	token := authToken(t, defaultUserID)

	w1 := env.postMessage(`{"content":"run the planned work"}`, token)
	require.Equal(t, http.StatusOK, w1.Code, "body: %s", w1.Body.String())
	types1, payloads1 := parseSSEBody(t, w1.Body.String())
	requireEventSubsequence(t, types1, []string{
		"tool_call", "plan_updated", "tool_result", "agent_start", "done",
	})

	planIndex := -1
	firstChildIndex := -1
	for i, typ := range types1 {
		if typ == stream.EventPlanUpdated && planIndex < 0 {
			planIndex = i
			require.Equal(t, stream.AgentConfucius, payloads1[i]["agent"])
			meta := payloads1[i]["meta"].(map[string]any)
			require.Equal(t, float64(1), meta[stream.MetaRevision])
			var snapshot struct {
				Revision int `json:"revision"`
				Steps    []struct {
					Step   string `json:"step"`
					Status string `json:"status"`
				} `json:"steps"`
			}
			require.NoError(t, json.Unmarshal([]byte(payloads1[i]["content"].(string)), &snapshot))
			require.Equal(t, 1, snapshot.Revision)
			require.Len(t, snapshot.Steps, 2)
			assert.Equal(t, agent.PlanStatusInProgress, snapshot.Steps[0].Status)
			assert.Equal(t, agent.PlanStatusInProgress, snapshot.Steps[1].Status)
		}
		if firstChildIndex < 0 && typ == stream.EventAgentStart && payloads1[i]["agent"] != stream.AgentConfucius {
			firstChildIndex = i
		}
	}
	require.GreaterOrEqual(t, planIndex, 0)
	require.Greater(t, firstChildIndex, planIndex)
	require.Contains(t, w1.Body.String(), "status: error")

	env.waitForPersistedTurn(t, defaultSessionID, 12)

	w2 := env.postMessage(`{"content":"continue"}`, token)
	require.Equal(t, http.StatusOK, w2.Code, "body: %s", w2.Body.String())
	types2, payloads2 := parseSSEBody(t, w2.Body.String())
	requireEventSubsequence(t, types2, []string{"plan_updated", "done"})
	var secondRevision float64
	for i, typ := range types2 {
		if typ == stream.EventPlanUpdated {
			secondRevision = payloads2[i]["meta"].(map[string]any)[stream.MetaRevision].(float64)
		}
	}
	assert.Equal(t, float64(1), secondRevision)

	env.waitForPersistedTurn(t, defaultSessionID, 20)
	var planRows []model.Message
	for _, msg := range env.mysqlFake.messagesFor(defaultSessionID) {
		if msg.EventType == model.EventTypePlanUpdated {
			planRows = append(planRows, msg)
		}
	}
	require.Len(t, planRows, 3)
	for _, msg := range planRows {
		assert.Empty(t, msg.Role)
		assert.NotEmpty(t, msg.Content)
	}
	assert.Contains(t, planRows[0].Content, `"revision":1`)
	assert.Contains(t, planRows[1].Content, `"revision":2`)
	assert.Contains(t, planRows[1].Content, `"status":"completed"`)
	assert.Contains(t, planRows[2].Content, `"revision":1`)
}

func TestDynamicSubagents_TotalBudgetExhaustionEndToEnd(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "budget_1",
				Function: agent.ToolCallFunction{
					Name: agent.SpawnSubagentTool, Arguments: `{"task":"one"}`,
				},
			}},
		},
		scriptedLLMResponse{content: "one result", finishReason: "stop"},
		scriptedLLMResponse{
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "budget_2",
				Function: agent.ToolCallFunction{
					Name: agent.SpawnSubagentTool, Arguments: `{"task":"two"}`,
				},
			}},
		},
		scriptedLLMResponse{content: "recovered after budget error", finishReason: "stop"},
	)
	agents := dynamicAgentsForTest(func(sub *config.SubAgentConfig) {
		sub.MaxTotalPerTurn = 1
	})
	env := newTestEnvWithAgents(t, llm, agents)
	w := env.postMessage(`{"content":"budget"}`, authToken(t, defaultUserID))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	types, payloads := parseSSEBody(t, w.Body.String())
	var sawBudgetError, sawDone bool
	var invocations []any
	for i, ty := range types {
		p := payloads[i]
		if ty == stream.EventAgentError && strings.Contains(p["content"].(string), "total per-turn budget exhausted") {
			sawBudgetError = true
		}
		if ty == stream.EventDone {
			sawDone = true
			usage := p["meta"].(map[string]any)["usage"].(map[string]any)
			invocations = usage["meta"].(map[string]any)["sub_agent_invocations"].([]any)
		}
	}
	require.True(t, sawBudgetError, "second spawn must return an in-band budget error")
	require.True(t, sawDone)
	require.Len(t, invocations, 1, "only the accepted spawn is counted")
}
