package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestDynamicSubagents_ResumeArgumentRejectedEndToEnd(t *testing.T) {
	llm := newScriptedLLMClient(
		// The model tries to resume an instance — the argument is no longer part
		// of the tool contract and must be rejected as bad_args without any
		// sub-agent LLM call or persisted instance.
		scriptedLLMResponse{
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "resume_call",
				Function: agent.ToolCallFunction{
					Name:      agent.SpawnSubagentTool,
					Arguments: `{"task":"continue work","resume_agent_id":"w-legacy"}`,
				},
			}},
		},
		scriptedLLMResponse{content: "summary", finishReason: "stop"},
	)
	env := newTestEnv(t, llm)

	w := env.postMessage(`{"content":"continue"}`, authToken(t, defaultUserID))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	env.waitForPersistedTurn(t, defaultSessionID, 3)

	var sawResult bool
	for _, msg := range env.mysqlFake.messagesFor(defaultSessionID) {
		if msg.Agent == stream.AgentConfucius && msg.EventType == model.EventTypeToolResult {
			sawResult = true
			assert.Contains(t, msg.Content, "resume_agent_id", "the rejection explains the unknown field")
		}
		// A rejected dispatch must never create a sub-agent LLM call, so no
		// sub-agent rows (which would carry a dynamic instance identity)
		// may appear.
		assert.Empty(t, msg.AgentInstanceID, "no sub-agent row may be persisted: %+v", msg)
	}
	require.True(t, sawResult, "the bad_args tool result must be persisted")

	llm.mu.Lock()
	defer llm.mu.Unlock()
	for _, call := range llm.calls {
		for _, m := range call.Messages {
			require.NotContains(t, m.Content, "continue work",
				"the rejected dispatch must not reach a sub-agent LLM call")
		}
	}
}

func TestDynamicSubagents_LegacyFullSnapshotStaysReadable(t *testing.T) {
	// A pre-change legacy_full row (no persisted message rows in `messages`,
	// only the snapshot) must remain readable through the per-run transcript
	// APIs — resume was removed, history retention was not. See the REMOVED
	// "Resume with persisted history" migration note in the spec delta.
	llm := newScriptedLLMClient(
		scriptedLLMResponse{content: "legacy summary", finishReason: "stop"},
	)
	env := newTestEnvWithAgents(t, llm, agentConfig())
	token := authToken(t, defaultUserID)
	now := time.Now().UTC()
	instance := model.SubAgentInstance{
		SessionID: defaultSessionID, AgentInstanceID: "w-legacy", Depth: 1,
		Name: "Legacy", SystemPrompt: "legacy system", ToolsJSON: []byte(`[]`),
	}
	legacyRun := model.SubAgentRun{
		SessionID: defaultSessionID, AgentInstanceID: "w-legacy",
		RunID: "legacy-call", RunNo: 1, Name: "Legacy",
		Status: model.SubAgentStatusCapped, SnapshotKind: model.SubAgentSnapshotLegacyFull,
		ResumeEligible: true, MessageCount: 3, ContextMessageCount: 2,
		ContextBytes: 120, StartedAt: now, FinishedAt: now,
		MessagesJSON: []byte(`[
			{"role":"system","content":"legacy system"},
			{"role":"user","content":"old task"},
			{"role":"assistant","content":"old result"}
		]`),
	}
	require.NoError(t, env.mysqlFake.AppendSubAgentRun(context.Background(), instance, legacyRun))

	history, ok, err := env.mysqlFake.GetSubAgentHistory(context.Background(), defaultSessionID, "w-legacy")
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, history.Runs, 1)
	assert.Equal(t, model.SubAgentSnapshotLegacyFull, history.Runs[0].SnapshotKind)

	listURL := "/api/v1/sessions/" + defaultSessionID + "/subagents/w-legacy/runs"
	req := httptest.NewRequest(http.MethodGet, listURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	listW := httptest.NewRecorder()
	env.engine.ServeHTTP(listW, req)
	require.Equal(t, http.StatusOK, listW.Code, listW.Body.String())
	assert.NotContains(t, listW.Body.String(), "messages_json")
	assert.NotContains(t, listW.Body.String(), "system_prompt")

	detailURL := listURL + "/legacy-call"
	req = httptest.NewRequest(http.MethodGet, detailURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	detailW := httptest.NewRecorder()
	env.engine.ServeHTTP(detailW, req)
	require.Equal(t, http.StatusOK, detailW.Code, detailW.Body.String())
	assert.Contains(t, detailW.Body.String(), "old task")
	assert.Contains(t, detailW.Body.String(), "old result")
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

// TestDynamicSubagents_UniqueNamesAndPlaceholderHistoryEndToEnd is the
// change-defining e2e (unique-subagent-message-placeholders): two same-name
// spawns produce two DISTINCT instances (unique suffixed names, both run_no=1),
// and the placeholder history view over HTTP retains the lifecycle markers and
// parent rows while omitting the children payload rows and the duplicated
// parent spawn tool_result — with the full transcript still lazy-loadable via
// the per-run detail API.
func TestDynamicSubagents_UniqueNamesAndPlaceholderHistoryEndToEnd(t *testing.T) {
	llm := newScriptedLLMClient(
		// Turn: Confucius dispatches two same-name spawns in parallel, then
		// answers. The scripted client replies identically to every sub-agent
		// call (one content response per dispatch).
		scriptedLLMResponse{
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{
				{ID: "spawn-a", Function: agent.ToolCallFunction{
					Name:      agent.SpawnSubagentTool,
					Arguments: `{"task":"draft the intro","name":"Writer"}`,
				}},
				{ID: "spawn-b", Function: agent.ToolCallFunction{
					Name:      agent.SpawnSubagentTool,
					Arguments: `{"task":"draft the outro","name":"Writer"}`,
				}},
			},
		},
		scriptedLLMResponse{content: "child draft", finishReason: "stop"},
		scriptedLLMResponse{content: "child draft", finishReason: "stop"},
		scriptedLLMResponse{content: "merged answer", finishReason: "stop"},
	)
	env := newTestEnv(t, llm)
	token := authToken(t, defaultUserID)

	w := env.postMessage(`{"content":"write both halves"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	types, payloads := parseSSEBody(t, w.Body.String())
	require.Contains(t, types, stream.EventDone)

	// The model-visible spawn results expose status only — no instance identity.
	var spawnResults []string
	for i, ty := range types {
		if ty != stream.EventToolResult {
			continue
		}
		content, _ := payloads[i]["content"].(string)
		if strings.Contains(content, "subagent_result") {
			spawnResults = append(spawnResults, content)
		}
	}
	require.Len(t, spawnResults, 2, "both spawn dispatches must answer with a tool_result")
	for _, res := range spawnResults {
		assert.NotContains(t, res, "agent_id", "spawn results carry no instance identity")
		assert.NotContains(t, res, "agent_instance_id")
	}

	// Two distinct instances with unique names, each with a single run_no=1 run
	// keyed by its parent spawn tool_call id.
	env.waitForPersistedTurn(t, defaultSessionID, 8)
	instances := env.mysqlFake.snapshotSubAgentInstances(defaultSessionID)
	require.Len(t, instances, 2, "two spawns produce exactly two instances")
	names := map[string]bool{}
	for _, inst := range instances {
		names[inst.Name] = true
	}
	require.Len(t, names, 2, "same-name spawns get unique display names: %v", instances)
	runsByID := env.mysqlFake.snapshotSubAgentRuns(defaultSessionID)
	require.Len(t, runsByID, 2, "two spawns produce exactly two runs")
	runA, ok := runsByID["spawn-a"]
	require.True(t, ok, "a run keyed by the spawn-a tool_call id must exist")
	runB, ok := runsByID["spawn-b"]
	require.True(t, ok, "a run keyed by the spawn-b tool_call id must exist")
	assert.Equal(t, 1, runA.RunNo)
	assert.Equal(t, 1, runB.RunNo)
	require.NotEqual(t, runA.AgentInstanceID, runB.AgentInstanceID)

	// Full history over HTTP: every persisted row is visible.
	fullW := env.getSessionMessages(defaultSessionID, "", token)
	require.Equal(t, http.StatusOK, fullW.Code, fullW.Body.String())
	full := decodeMessagesPage(t, fullW)
	require.NotEmpty(t, full.Messages)

	// Placeholder history over HTTP: the children payload rows and both
	// duplicated parent spawn tool_result rows are omitted; the parent
	// tool_call rows, the lifecycle markers, the user row, and Confucius's
	// final answer stay.
	phW := env.getSessionMessages(defaultSessionID, "?subagent_content=placeholder", token)
	require.Equal(t, http.StatusOK, phW.Code, phW.Body.String())
	ph := decodeMessagesPage(t, phW)
	require.NotEmpty(t, ph.Messages)

	spawnToolCallSeen := map[string]bool{"spawn-a": false, "spawn-b": false}
	var sawUser bool
	var childStarts, childEnds int
	var sawConfuciusStart, sawConfuciusEnd bool
	for _, m := range ph.Messages {
		if m.AgentInstanceID != "" {
			assert.Contains(t,
				[]string{model.EventTypeAgentStart, model.EventTypeAgentEnd, model.EventTypeAgentError},
				m.EventType,
				"instance-attributed placeholder rows are lifecycle markers only: %+v", m)
		}
		switch m.EventType {
		case model.EventTypeAgentStart:
			if m.AgentInstanceID != "" {
				childStarts++
			} else if m.Agent == stream.AgentConfucius {
				sawConfuciusStart = true
			}
		case model.EventTypeAgentEnd:
			if m.AgentInstanceID != "" {
				childEnds++
			} else if m.Agent == stream.AgentConfucius {
				sawConfuciusEnd = true
			}
		case model.EventTypeMessage:
			sawUser = true
		case model.EventTypeToolCall:
			var payload struct {
				ToolCallID string `json:"tool_call_id"`
			}
			if json.Unmarshal([]byte(m.Content), &payload) == nil {
				if _, ok := spawnToolCallSeen[payload.ToolCallID]; ok {
					spawnToolCallSeen[payload.ToolCallID] = true
				}
			}
		case model.EventTypeToolResult:
			assert.NotContains(t, m.Content, "subagent_result",
				"the duplicated parent spawn result must be omitted")
		}
	}
	assert.Equal(t, 2, childStarts, "both children's agent_start markers stay visible")
	assert.Equal(t, 2, childEnds, "both children's agent_end markers stay visible")
	assert.True(t, sawConfuciusStart && sawConfuciusEnd, "the parent's lifecycle markers stay visible")
	assert.True(t, sawUser, "the user row stays visible")
	assert.True(t, spawnToolCallSeen["spawn-a"] && spawnToolCallSeen["spawn-b"],
		"the parent spawn tool_call rows stay visible")

	// Placeholder mode must strictly shrink the view.
	assert.Less(t, len(ph.Messages), len(full.Messages))

	// An unknown mode is a 400 before any pagination read.
	badW := env.getSessionMessages(defaultSessionID, "?subagent_content=bogus", token)
	assert.Equal(t, http.StatusBadRequest, badW.Code)
	assert.Contains(t, badW.Body.String(), "INVALID_SUBAGENT_CONTENT")

	// The detail the placeholder view hides stays lazy-loadable per run.
	for _, r := range []model.SubAgentRun{runA, runB} {
		listURL := "/api/v1/sessions/" + defaultSessionID + "/subagents/" + r.AgentInstanceID + "/runs"
		req := httptest.NewRequest(http.MethodGet, listURL, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		lw := httptest.NewRecorder()
		env.engine.ServeHTTP(lw, req)
		require.Equal(t, http.StatusOK, lw.Code, lw.Body.String())
		assert.NotContains(t, lw.Body.String(), "messages_json", "the list stays a summary")

		detailURL := listURL + "/" + r.RunID
		req = httptest.NewRequest(http.MethodGet, detailURL, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		dw := httptest.NewRecorder()
		env.engine.ServeHTTP(dw, req)
		require.Equal(t, http.StatusOK, dw.Code, dw.Body.String())
		assert.Contains(t, dw.Body.String(), "child draft", "the run detail lazy-loads the full transcript")
	}
}

// decodeMessagesPage parses the getSessionMessagesResponse body.
func decodeMessagesPage(t *testing.T, w *httptest.ResponseRecorder) struct {
	Messages      []model.Message `json:"messages"`
	NextPageToken string          `json:"next_page_token"`
} {
	t.Helper()
	var page struct {
		Messages      []model.Message `json:"messages"`
		NextPageToken string          `json:"next_page_token"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	return page
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
