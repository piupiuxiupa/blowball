package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modelCatalogConfig builds a config carrying the two-entry openai.models
// catalog (per-request-model-selection / model-effort-v2): a thinking default
// and a non-thinking alternative. defaultReasoningEffort overrides the
// deployment effort default (empty → none).
func modelCatalogConfig(defaultReasoningEffort string) *config.Config {
	openai := config.OpenAIConfig{
		APIKey: "test",
		Models: []config.ModelCatalogEntry{
			{Name: "gpt-5", MaxContextTokens: 400000, Thinking: true},
			{Name: "glm-4.7", MaxContextTokens: 200000, Thinking: false},
		},
		DefaultModel:           "gpt-5",
		DefaultReasoningEffort: defaultReasoningEffort,
	}
	if openai.DefaultReasoningEffort == "" {
		openai.DefaultReasoningEffort = "none"
	}
	return &config.Config{
		OpenAI: openai,
		JWT:    config.JWTConfig{Secret: integrationTestSecret, Expire: "1h"},
		Agents: agentConfig(),
	}
}

// dispatchAllScript is the 4-round LLM script that drives Confucius to
// dispatch BOTH sub-agents in one parallel round and then finish, so the test
// observes one LLM call from every agent in the tree.
func dispatchAllScript() []scriptedLLMResponse {
	return []scriptedLLMResponse{
		{
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{
				{ID: "c1", Function: agent.ToolCallFunction{Name: agent.SpawnSubagentTool, Arguments: `{"task":"t1"}`}},
				{ID: "c2", Function: agent.ToolCallFunction{Name: agent.SpawnSubagentTool, Arguments: `{"task":"t2"}`}},
			},
			usage: agent.Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
		},
		{tokens: []string{"chongzhi-done"}, content: "chongzhi-done", finishReason: "stop", usage: agent.Usage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22}},
		{tokens: []string{"liang-done"}, content: "liang-done", finishReason: "stop", usage: agent.Usage{PromptTokens: 30, CompletionTokens: 3, TotalTokens: 33}},
		{tokens: []string{"final"}, content: "final", finishReason: "stop", usage: agent.Usage{PromptTokens: 40, CompletionTokens: 4, TotalTokens: 44}},
	}
}

// TestPerRequestModel_NonThinkingSelectionDrivesAllAgents posts a turn that
// selects the non-thinking catalog model and verifies EVERY LLM call of the
// turn — Confucius's two rounds plus both dispatched sub-agents — runs on the
// selected model with the non-thinking wire family, and the turn_usage row
// records it.
func TestPerRequestModel_NonThinkingSelectionDrivesAllAgents(t *testing.T) {
	llm := newScriptedLLMClient(dispatchAllScript()...)
	env := newTestEnvWithConfig(t, llm, modelCatalogConfig(""))

	w := env.postMessage(`{"content":"hi","model":"glm-4.7"}`, authToken(t, defaultUserID))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	reqs := llm.turnRequests()
	require.Len(t, reqs, 4, "want Confucius×2 + Chongzhi + Liang LLM calls")
	for i, r := range reqs {
		assert.Equal(t, "glm-4.7", r.Model, "call %d model", i)
		assert.False(t, r.Thinking, "call %d must run the non-thinking wire family", i)
	}

	// The turn's resolved model lands in turn_usage (per-model cost
	// aggregation).
	require.Eventually(t, func() bool {
		return len(env.mysqlFake.turnUsagesFor(defaultSessionID)) == 1
	}, 2e9, 10e6, "expected one turn_usage row")
	tu := env.mysqlFake.turnUsagesFor(defaultSessionID)[0]
	assert.Equal(t, "glm-4.7", tu.Model)
}

// TestPerRequestModel_NoParamsResolveToDeploymentDefaults covers the
// dual-axis default case: a parameter-less turn resolves BOTH axes — the
// default catalog entry + the deployment default effort (model-effort-v2).
func TestPerRequestModel_NoParamsResolveToDeploymentDefaults(t *testing.T) {
	llm := newScriptedLLMClient(dispatchAllScript()...)
	env := newTestEnvWithConfig(t, llm, modelCatalogConfig("xhigh"))

	w := env.postMessage(`{"content":"hi"}`, authToken(t, defaultUserID))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	reqs := llm.turnRequests()
	require.Len(t, reqs, 4)
	for i, r := range reqs {
		assert.Equal(t, "gpt-5", r.Model, "call %d model (default entry)", i)
		assert.True(t, r.Thinking, "call %d wire family", i)
		assert.Equal(t, "xhigh", r.ReasoningEffort, "call %d effort (deployment default)", i)
	}
}

// TestPerRequestModel_NoneEffortSentLiterallyOnThinkingEntry covers the B2
// wire family's core fix: a thinking entry with effort "none" keeps the
// thinking family and carries the literal "none" — the OpenAI client then
// SENDS reasoning_effort:"none" instead of omitting the parameter (the
// glm keeps-thinking-on-its-own-default bug).
func TestPerRequestModel_NoneEffortSentLiterallyOnThinkingEntry(t *testing.T) {
	llm := newScriptedLLMClient(dispatchAllScript()...)
	env := newTestEnvWithConfig(t, llm, modelCatalogConfig("high"))

	w := env.postMessage(`{"content":"hi","reasoning_effort":"none"}`, authToken(t, defaultUserID))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	reqs := llm.turnRequests()
	require.Len(t, reqs, 4)
	for i, r := range reqs {
		assert.Equal(t, "gpt-5", r.Model, "call %d model", i)
		assert.True(t, r.Thinking, "call %d must STAY on the thinking wire family", i)
		assert.Equal(t, "none", r.ReasoningEffort, "call %d effort must stay the literal none", i)
	}
}

// TestPerRequestModel_DeploymentDefaultClampedOnNonThinkingEntry: a non-none
// DEPLOYMENT default landing on a thinking:false entry is clamped to none
// (WARN) and the turn proceeds — no 400 (spec: "部署默认撞上非思考条目时钳制").
func TestPerRequestModel_DeploymentDefaultClampedOnNonThinkingEntry(t *testing.T) {
	llm := newScriptedLLMClient(dispatchAllScript()...)
	env := newTestEnvWithConfig(t, llm, modelCatalogConfig("high"))

	w := env.postMessage(`{"content":"hi","model":"glm-4.7"}`, authToken(t, defaultUserID))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	reqs := llm.turnRequests()
	require.Len(t, reqs, 4)
	for i, r := range reqs {
		assert.Equal(t, "glm-4.7", r.Model, "call %d model", i)
		assert.False(t, r.Thinking, "call %d wire family", i)
	}
}

// TestPerRequestModel_ThinkingSelectionWithEffort posts a turn selecting the
// thinking model with an explicit effort and verifies the uniform turn config
// in the thinking direction.
func TestPerRequestModel_ThinkingSelectionWithEffort(t *testing.T) {
	llm := newScriptedLLMClient(dispatchAllScript()...)
	env := newTestEnvWithConfig(t, llm, modelCatalogConfig(""))

	w := env.postMessage(`{"content":"hi","model":"gpt-5","reasoning_effort":"high"}`, authToken(t, defaultUserID))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	reqs := llm.turnRequests()
	require.Len(t, reqs, 4)
	for i, r := range reqs {
		assert.Equal(t, "gpt-5", r.Model, "call %d model", i)
		assert.True(t, r.Thinking, "call %d thinking", i)
		assert.Equal(t, "high", r.ReasoningEffort, "call %d effort", i)
	}
}

// TestPerRequestModel_UnknownModelRejected verifies a name outside the catalog
// is a 400 INVALID_MODEL on the real route.
func TestPerRequestModel_UnknownModelRejected(t *testing.T) {
	env := newTestEnvWithConfig(t, newScriptedLLMClient(), modelCatalogConfig(""))

	w := env.postMessage(`{"content":"hi","model":"gpt-99"}`, authToken(t, defaultUserID))
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "INVALID_MODEL")
}

// TestPerRequestModel_EffortOnNonThinkingModelRejected verifies the
// model-capability gate on the real route: a non-"none" effort EXPLICITLY
// requested on the thinking:false catalog entry is a 400 INVALID_EFFORT.
func TestPerRequestModel_EffortOnNonThinkingModelRejected(t *testing.T) {
	env := newTestEnvWithConfig(t, newScriptedLLMClient(), modelCatalogConfig(""))

	w := env.postMessage(`{"content":"hi","model":"glm-4.7","reasoning_effort":"high"}`, authToken(t, defaultUserID))
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "INVALID_EFFORT")
}

// TestPerRequestModel_ModelsEndpoint verifies GET /api/v1/models on the
// full-route engine returns the catalog, default, and deployment default
// effort, JWT-gated.
func TestPerRequestModel_ModelsEndpoint(t *testing.T) {
	env := newTestEnvWithConfig(t, newScriptedLLMClient(), modelCatalogConfig("high"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+authToken(t, defaultUserID))
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body struct {
		Models []struct {
			Name             string `json:"name"`
			MaxContextTokens int    `json:"max_context_tokens"`
			Thinking         bool   `json:"thinking"`
		} `json:"models"`
		Default              string `json:"default"`
		DefaultReasoningEfft string `json:"default_reasoning_effort"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Models, 2)
	assert.Equal(t, "gpt-5", body.Models[0].Name)
	assert.Equal(t, 400000, body.Models[0].MaxContextTokens)
	assert.True(t, body.Models[0].Thinking)
	assert.Equal(t, "glm-4.7", body.Models[1].Name)
	assert.False(t, body.Models[1].Thinking)
	assert.Equal(t, "gpt-5", body.Default)
	assert.Equal(t, "high", body.DefaultReasoningEfft)

	// Unauthenticated: the route sits behind AuthMiddleware.
	noAuth := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	w2 := httptest.NewRecorder()
	env.engine.ServeHTTP(w2, noAuth)
	assert.Equal(t, http.StatusUnauthorized, w2.Code)
}
