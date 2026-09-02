package integration

import (
	"encoding/json"
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
