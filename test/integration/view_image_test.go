package integration

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool/xizhi"
)

// viewImagePNG is a magic-byte-valid PNG payload (the sidecar sniffs content,
// it never decodes).
var viewImagePNG = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, []byte("fake-png-bytes")...)

const (
	viewImageModel       = "vision-model"
	viewImageRelPath     = "shots/err.png"
	viewImageQuestion    = "what is the error stack in this screenshot?"
	viewImageDescription = "The screenshot shows a nil pointer dereference at main.go:42."
)

func viewImageAgentsConfig() config.AgentsConfig {
	cfg := agentConfig()
	cfg.Subagent.Tools = append([]string{xizhi.NameViewImage}, cfg.Subagent.Tools...)
	return cfg
}

func viewImageTestConfig() *config.Config {
	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{
			APIKey: "test",
			Models: append(testCatalog(), config.ModelCatalogEntry{
				Name:                viewImageModel,
				MaxContextTokens:    8192,
				MaxCompletionTokens: 2048,
				Thinking:            true, // lets the agent pick a reasoning effort per call
			}),
		},
		JWT:    config.JWTConfig{Secret: integrationTestSecret, Expire: "1h"},
		Agents: viewImageAgentsConfig(),
	}
	cfg.Tools.Xizhi.ViewImage = config.ViewImageConfig{Enabled: true, Model: viewImageModel}
	return cfg
}

func seedWorkspaceImage(t *testing.T, env *testEnv) {
	t.Helper()
	ws := env.fsSvc.UserWorkspace(defaultUserID)
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "shots"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, viewImageRelPath), viewImagePNG, 0o644))
}

// findVisionCall returns the scripted request the vision sidecar issued.
func findVisionCall(t *testing.T, llm *scriptedLLMClient) agent.LLMRequest {
	t.Helper()
	for _, req := range llm.requests() {
		if req.Model == viewImageModel {
			return req
		}
	}
	t.Fatalf("no %s call recorded among %d calls", viewImageModel, len(llm.requests()))
	return agent.LLMRequest{}
}

// TestViewImage_ToolTurnDescribesWorkspaceImage drives the full vision sidecar
// path through the real orchestrator: Confucius dispatches Chongzhi, Chongzhi
// calls xizhi_view_image, the tool reads the workspace image and issues ONE
// designated-model call, and the description flows back as the tool-result
// envelope while the turn completes normally.
func TestViewImage_ToolTurnDescribesWorkspaceImage(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{ // Confucius: dispatch Chongzhi.
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "call_invoke_c",
				Function: agent.ToolCallFunction{
					Name:      agent.SpawnSubagentTool,
					Arguments: `{"task":"describe the error screenshot","context":"user asked"}`,
				},
			}},
			usage: agent.Usage{PromptTokens: 20, CompletionTokens: 1, TotalTokens: 21},
		},
		scriptedLLMResponse{ // Chongzhi: call the vision tool.
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "call_view",
				Function: agent.ToolCallFunction{
					Name:      xizhi.NameViewImage,
					Arguments: `{"path":` + mustMarshalJSONString(viewImageRelPath) + `,"question":` + mustMarshalJSONString(viewImageQuestion) + `,"reasoning_effort":"high"}`,
				},
			}},
			usage: agent.Usage{PromptTokens: 30, CompletionTokens: 1, TotalTokens: 31},
		},
		scriptedLLMResponse{ // Vision model: text description.
			content:      viewImageDescription,
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 200, CompletionTokens: 50, TotalTokens: 250},
		},
		scriptedLLMResponse{ // Chongzhi: summarize.
			content:      "The screenshot shows a nil pointer dereference.",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 35, CompletionTokens: 2, TotalTokens: 37},
		},
		scriptedLLMResponse{ // Confucius: final answer.
			content:      "Your screenshot shows a nil pointer dereference at main.go:42.",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 40, CompletionTokens: 3, TotalTokens: 43},
		},
	)
	env := newTestEnvWithConfig(t, llm, viewImageTestConfig())
	seedWorkspaceImage(t, env)
	token := authToken(t, defaultUserID)

	w := env.postMessage(`{"content":"what is wrong in my screenshot shots/err.png?"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "sse body: %s", w.Body.String())
	types, payloads := parseSSEBody(t, w.Body.String())
	requireEventPresent(t, types, stream.EventDone)

	// The sidecar issued exactly the designated vision model call carrying the
	// tool's question as the user text.
	visionReq := findVisionCall(t, llm)
	require.Len(t, visionReq.Messages, 1)
	assert.Equal(t, "user", visionReq.Messages[0].Role)
	assert.Equal(t, viewImageQuestion, visionReq.Messages[0].Content)
	assert.True(t, visionReq.Thinking, "agent-chosen high effort must select the thinking wire family")
	assert.Equal(t, "high", visionReq.ReasoningEffort)

	// The persisted tool interaction carries the success envelope with the
	// description and image metadata.
	// The streamed tool_result for xizhi_view_image carries the success
	// envelope with the description and image metadata. (Dynamic sub-agent
	// tool calls stream through SSE but are not written to the shared
	// tool_calls table — they live in the sub-agent transcript store — so the
	// event stream is the integration surface asserted here.)
	viewResult := findToolResultPayload(t, payloads, "call_view")
	var envelope struct {
		Status int `json:"status"`
		Result struct {
			Path        string `json:"path"`
			MIMEType    string `json:"mime_type"`
			Model       string `json:"model"`
			Description string `json:"description"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal([]byte(viewResult), &envelope), "tool result must be envelope JSON, got %s", viewResult)
	assert.Equal(t, 0, envelope.Status)
	assert.Equal(t, viewImageRelPath, envelope.Result.Path)
	assert.Equal(t, "image/png", envelope.Result.MIMEType)
	assert.Equal(t, viewImageModel, envelope.Result.Model)
	assert.Equal(t, viewImageDescription, envelope.Result.Description)
}

// TestViewImage_VisionFailureDoesNotBreakTurn verifies a failing vision model
// degrades to a tool error envelope while the main conversation completes.
func TestViewImage_VisionFailureDoesNotBreakTurn(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{ // Confucius: dispatch.
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "call_invoke_c",
				Function: agent.ToolCallFunction{
					Name:      agent.SpawnSubagentTool,
					Arguments: `{"task":"describe the screenshot","context":""}`,
				},
			}},
			usage: agent.Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
		},
		scriptedLLMResponse{ // Chongzhi: call the vision tool.
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "call_view",
				Function: agent.ToolCallFunction{
					Name:      xizhi.NameViewImage,
					Arguments: `{"path":` + mustMarshalJSONString(viewImageRelPath) + `}`,
				},
			}},
			usage: agent.Usage{PromptTokens: 20, CompletionTokens: 1, TotalTokens: 21},
		},
		scriptedLLMResponse{ // Vision model fails hard.
			err: assert.AnError,
		},
		scriptedLLMResponse{ // Chongzhi: recover with a stop despite the tool error.
			content:      "The image tool failed, but the turn continues.",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 30, CompletionTokens: 2, TotalTokens: 32},
		},
		scriptedLLMResponse{ // Confucius: final answer.
			content:      "Image analysis is unavailable right now.",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 35, CompletionTokens: 3, TotalTokens: 38},
		},
	)
	env := newTestEnvWithConfig(t, llm, viewImageTestConfig())
	seedWorkspaceImage(t, env)
	token := authToken(t, defaultUserID)

	w := env.postMessage(`{"content":"what is in shots/err.png?"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "sse body: %s", w.Body.String())
	types, payloads := parseSSEBody(t, w.Body.String())
	requireEventPresent(t, types, stream.EventDone)

	viewResult := findToolResultPayload(t, payloads, "call_view")
	var envelope struct {
		Status int    `json:"status"`
		Error  string `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(viewResult), &envelope))
	assert.Equal(t, 1, envelope.Status)
	assert.Contains(t, envelope.Error, "vision model call")
}

// findToolResultPayload returns the content string of the tool_result event
// whose meta.tool_call_id matches id.
func findToolResultPayload(t *testing.T, payloads []map[string]any, id string) string {
	t.Helper()
	for _, p := range payloads {
		if p["type"] != stream.EventToolResult {
			continue
		}
		meta, _ := p["meta"].(map[string]any)
		if meta == nil || meta["tool_call_id"] != id {
			continue
		}
		content, _ := p["content"].(string)
		require.NotEmpty(t, content, "tool_result content for %s must not be empty", id)
		return content
	}
	t.Fatalf("no tool_result event for tool_call_id %s", id)
	return ""
}

func mustMarshalJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
