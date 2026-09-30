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
	"github.com/lush/blowball/internal/service"
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
	env.waitForPersistedTurn(t, defaultSessionID, 2)

	var sawResult bool
	for _, call := range env.mysqlFake.toolCallsFor(defaultSessionID) {
		if call.ToolCallID == "resume_call" {
			sawResult = true
			assert.Contains(t, call.Result, "resume_agent_id", "the rejection explains the unknown field")
		}
	}
	require.True(t, sawResult, "the bad_args tool result must be persisted")

	// A rejected dispatch must never create a sub-agent LLM call, so no
	// sub-agent rows (dynamic instance identity) may appear in messages.
	for _, msg := range env.mysqlFake.messagesFor(defaultSessionID) {
		assert.Empty(t, msg.AgentInstanceID, "no sub-agent row may be persisted: %+v", msg)
	}

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

	env.waitForPersistedTurn(t, defaultSessionID, 2)

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

	// Plan snapshots are control-plane state, not message rows
	// (refactor-message-storage): their revisions were already asserted from
	// the live SSE stream above. Messages hold finals + tool placeholders.
	env.waitForPersistedTurn(t, defaultSessionID, 4)
	for _, msg := range env.mysqlFake.messagesFor(defaultSessionID) {
		assert.Contains(t, []string{model.EventTypeMessage, model.EventTypeToolCall}, msg.EventType,
			"only finals and placeholders persist: got %s/%s", msg.Agent, msg.EventType)
	}
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
	env.waitForPersistedTurn(t, defaultSessionID, 2)
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

	var sawUser bool
	var spawnLinks int
	for _, m := range ph.Messages {
		if m.ToolCallID != "" && m.AgentInstanceID != "" {
			spawnLinks++
		}
		switch m.EventType {
		case model.EventTypeMessage:
			sawUser = true
		}
	}
	// Post refactor-message-storage new data writes NO sub-agent rows into
	// messages at all (their transcripts live in subagent_runs, lazy-loaded
	// via the run APIs), and no marker/tool event rows either — so the
	// placeholder and full views coincide, and the parent's lifecycle shows
	// only through its user + round-final rows.
	assert.True(t, sawUser, "the user row stays visible")
	assert.Equal(t, 2, spawnLinks, "both spawn placeholders stay visible with their instance links")
	assert.Equal(t, len(ph.Messages), len(full.Messages),
		"placeholder and full views coincide when no sub-agent content rows exist")

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

// getSubAgentJSON issues an authenticated sub-agent API GET and decodes the
// JSON body into out.
func getSubAgentJSON(t *testing.T, env *testEnv, path, token string, out any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), out))
	return w
}

func TestDynamicSubagents_NestedRunLinksOverHTTP(t *testing.T) {
	llm := newScriptedLLMClient(
		// Confucius spawns the depth-1 instance.
		scriptedLLMResponse{
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "spawn-a",
				Function: agent.ToolCallFunction{
					Name:      agent.SpawnSubagentTool,
					Arguments: `{"task":"outer work","name":"Outer"}`,
				},
			}},
		},
		// The depth-1 instance spawns a depth-2 grandchild.
		scriptedLLMResponse{
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "spawn-b",
				Function: agent.ToolCallFunction{
					Name:      agent.SpawnSubagentTool,
					Arguments: `{"task":"inner work","name":"Inner"}`,
				},
			}},
		},
		scriptedLLMResponse{content: "grandchild result", finishReason: "stop"},
		scriptedLLMResponse{content: "outer done", finishReason: "stop"},
		scriptedLLMResponse{content: "merged answer", finishReason: "stop"},
	)
	env := newTestEnvWithAgents(t, llm, dynamicAgentsForTest(func(c *config.SubAgentConfig) {
		c.MaxDepth = 2
	}))
	token := authToken(t, defaultUserID)

	w := env.postMessage(`{"content":"delegate twice"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	env.waitForPersistedTurn(t, defaultSessionID, 2)

	instances := env.mysqlFake.snapshotSubAgentInstances(defaultSessionID)
	require.Len(t, instances, 2, "one depth-1 and one depth-2 instance must persist")
	var outer, inner model.SubAgentInstance
	for _, inst := range instances {
		if inst.ParentInstanceID == "" {
			outer = inst
		}
	}
	for _, inst := range instances {
		if inst.AgentInstanceID != outer.AgentInstanceID && inst.ParentInstanceID == outer.AgentInstanceID {
			inner = inst
		}
	}
	require.NotEmpty(t, outer.AgentInstanceID, "depth-1 instance not found")
	require.NotEmpty(t, inner.AgentInstanceID, "depth-2 instance not found")
	assert.Equal(t, 2, inner.Depth)

	// Root run listing omits parent_instance_id entirely.
	var outerList struct {
		Runs []service.SubAgentRunSummary `json:"runs"`
	}
	listW := getSubAgentJSON(t, env,
		"/api/v1/sessions/"+defaultSessionID+"/subagents/"+outer.AgentInstanceID+"/runs",
		token, &outerList)
	require.Len(t, outerList.Runs, 1)
	assert.Equal(t, "spawn-a", outerList.Runs[0].RunID)
	assert.Empty(t, outerList.Runs[0].ParentInstanceID)
	assert.NotContains(t, listW.Body.String(), "parent_instance_id")

	// The depth-1 run detail links its nested spawn tool call to the depth-2
	// instance — the only durable way for the frontend to discover it.
	var outerDetail service.SubAgentRunDetail
	getSubAgentJSON(t, env,
		"/api/v1/sessions/"+defaultSessionID+"/subagents/"+outer.AgentInstanceID+"/runs/spawn-a",
		token, &outerDetail)
	var linked *service.SubAgentToolCall
	for i := range outerDetail.Transcript {
		for j := range outerDetail.Transcript[i].ToolCalls {
			call := outerDetail.Transcript[i].ToolCalls[j]
			if call.ID == "spawn-b" {
				linked = &outerDetail.Transcript[i].ToolCalls[j]
			}
		}
	}
	require.NotNil(t, linked, "the nested spawn tool call must appear in the transcript")
	assert.Equal(t, agent.SpawnSubagentTool, linked.Name)
	assert.Equal(t, inner.AgentInstanceID, linked.SubAgentInstanceID)
	assert.Equal(t, inner.Name, linked.SubAgentName)

	// The discovered id resolves through the same run APIs one level deeper.
	var innerList struct {
		Runs []service.SubAgentRunSummary `json:"runs"`
	}
	getSubAgentJSON(t, env,
		"/api/v1/sessions/"+defaultSessionID+"/subagents/"+inner.AgentInstanceID+"/runs",
		token, &innerList)
	require.Len(t, innerList.Runs, 1)
	assert.Equal(t, "spawn-b", innerList.Runs[0].RunID)
	assert.Equal(t, outer.AgentInstanceID, innerList.Runs[0].ParentInstanceID)

	var innerDetail service.SubAgentRunDetail
	getSubAgentJSON(t, env,
		"/api/v1/sessions/"+defaultSessionID+"/subagents/"+inner.AgentInstanceID+"/runs/spawn-b",
		token, &innerDetail)
	assert.Equal(t, outer.AgentInstanceID, innerDetail.ParentInstanceID)
	foundGrandchild := false
	for _, item := range innerDetail.Transcript {
		if strings.Contains(item.Content, "grandchild result") {
			foundGrandchild = true
		}
	}
	assert.True(t, foundGrandchild, "the depth-2 transcript lazy-loads its own content")
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
