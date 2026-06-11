package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool/xizhi"
)

// TestFileOps_UploadThenAgentWritesThenListAndRead drives the full workspace
// round-trip:
//  1. POST /api/v1/workspace/upload writes a small file to the user's
//     workspace via the real multipart handler; the file must exist on disk.
//  2. POST /api/v1/sessions/:id/messages triggers a Confuse turn that
//     dispatches to Chongzhi, which calls xizhi_write_file to create a NEW
//     file inside the workspace. The SSE stream must complete and the file
//     must appear on disk under the user's workspace.
//  3. GET /api/v1/workspace/files lists the workspace; both the uploaded
//     file and the agent-written file must appear.
//  4. GET /api/v1/workspace/files/<agent-path>/content returns the agent-
//     written file's text content.
//
// Components on the critical path (in addition to those in message_flow_test):
//   - handler.WorkspaceHandler Upload / List / Content
//   - fs.Store UserWorkspace path resolution
//   - xizhi.ValidatePath security primitive (exercised on every WS route)
//   - real xizhi.WriteFile invoked through the tool registry by Chongzhi's
//     tool-calling loop
//   - agent.Confuse → invoke_chongzhi dispatch with isolated context
//   - agent.Chongzhi tool-calling loop with finish_reason="tool_calls" then
//     finish_reason="stop"
func TestFileOps_UploadThenAgentWritesThenListAndRead(t *testing.T) {
	const (
		uploadedName  = "notes.txt"
		uploadedBody  = "uploaded body"
		agentFilePath = "agent_output.md"
		agentFileBody = "# Agent Output\n\nwritten by Chongzhi"
		agentSummary  = "I created agent_output.md in your workspace."
	)

	// Script:
	//  1. Confuse round 1: invoke_chongzhi with a task asking to write the file.
	//  2. Chongzhi round 1: emit xizhi_write_file tool_call.
	//  3. Chongzhi round 2: confirm with a stop summary.
	//  4. Confuse round 2: produce the final summary.
	//  5. TitleService round (fires asynchronously).
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "call_invoke_c",
				Function: agent.ToolCallFunction{
					Name:      agent.ToolInvokeChongzhi,
					Arguments: `{"task":"write a markdown summary to ` + agentFilePath + `","context":"user asked for a file"}`,
				},
			}},
			usage: agent.Usage{PromptTokens: 20, CompletionTokens: 1, TotalTokens: 21},
		},
		scriptedLLMResponse{
			finishReason: "tool_calls",
			toolCalls: []agent.ToolCall{{
				ID: "call_write",
				Function: agent.ToolCallFunction{
					Name:      xizhi.NameWriteFile,
					Arguments: mustMarshalWriteArgs(t, agentFilePath, agentFileBody),
				},
			}},
			usage: agent.Usage{PromptTokens: 30, CompletionTokens: 1, TotalTokens: 31},
		},
		scriptedLLMResponse{
			tokens:       []string{"done"},
			content:      "wrote " + agentFilePath,
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 35, CompletionTokens: 1, TotalTokens: 36},
		},
		scriptedLLMResponse{
			tokens:       []string{agentSummary},
			content:      agentSummary,
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 42},
		},
		scriptedLLMResponse{
			content:      "FileOps",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	)
	env := newTestEnv(t, llm)
	token := authToken(t, defaultUserID)

	// (1) Upload a small file.
	uploadBody := &bytes.Buffer{}
	writer := multipart.NewWriter(uploadBody)
	part, err := writer.CreateFormFile("file", uploadedName)
	require.NoError(t, err)
	_, err = io.Copy(part, bytes.NewReader([]byte(uploadedBody)))
	require.NoError(t, err)
	require.NoError(t, writer.WriteField("path", ""))
	require.NoError(t, writer.Close())

	upReq := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/upload", uploadBody)
	upReq.Header.Set("Content-Type", writer.FormDataContentType())
	upReq.Header.Set("Authorization", "Bearer "+token)
	upW := httptest.NewRecorder()
	env.engine.ServeHTTP(upW, upReq)

	require.Equal(t, http.StatusOK, upW.Code, "upload body: %s", upW.Body.String())
	var uploadResp struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	require.NoError(t, json.Unmarshal(upW.Body.Bytes(), &uploadResp))
	assert.Equal(t, uploadedName, uploadResp.Path)
	assert.Equal(t, int64(len(uploadedBody)), uploadResp.Size)

	uploadedDisk := filepath.Join(env.dataDir, defaultUserID, "workspace", uploadedName)
	got, err := os.ReadFile(uploadedDisk)
	require.NoError(t, err, "uploaded file must exist on disk")
	assert.Equal(t, uploadedBody, string(got))

	// (2) Send the chat message that triggers Chongzhi → xizhi_write_file.
	w := env.postMessage(`{"content":"please write a markdown summary file"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "sse body: %s", w.Body.String())
	require.Equal(t, "text/event-stream", w.Result().Header.Get("Content-Type"))

	types, _ := parseSSEBody(t, w.Body.String())
	require.NotEmpty(t, types)
	requireEventPresent(t, types, stream.EventAgentStart) // at least one agent started
	requireEventPresent(t, types, stream.EventDone)       // stream terminated

	// The agent-written file must exist on disk under the user's workspace.
	// The Chongzhi tool-call executes synchronously inside the agent loop
	// before the SSE response completes, so by the time we get here the file
	// is already on disk.
	agentDisk := filepath.Join(env.dataDir, defaultUserID, "workspace", agentFilePath)
	_, err = os.Stat(agentDisk)
	require.NoError(t, err, "agent-written file must exist on disk")
	agentData, err := os.ReadFile(agentDisk)
	require.NoError(t, err, "agent-written file must exist on disk")
	assert.Equal(t, agentFileBody, string(agentData))

	// (3) List the workspace; both files must appear.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files", nil)
	listReq.Header.Set("Authorization", "Bearer "+token)
	listW := httptest.NewRecorder()
	env.engine.ServeHTTP(listW, listReq)

	require.Equal(t, http.StatusOK, listW.Code, "list body: %s", listW.Body.String())
	var listResp struct {
		Files []struct {
			Name string `json:"name"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"files"`
	}
	require.NoError(t, json.Unmarshal(listW.Body.Bytes(), &listResp))
	names := map[string]bool{}
	for _, f := range listResp.Files {
		names[f.Name] = true
	}
	assert.True(t, names[uploadedName], "uploaded file must be listed; got %v", names)
	assert.True(t, names[agentFilePath], "agent-written file must be listed; got %v", names)

	// (4) GET .../content returns the agent-written file's text body.
	contentReq := httptest.NewRequest(http.MethodGet,
		"/api/v1/workspace/files/"+agentFilePath+"/content", nil)
	contentReq.Header.Set("Authorization", "Bearer "+token)
	contentW := httptest.NewRecorder()
	env.engine.ServeHTTP(contentW, contentReq)

	require.Equal(t, http.StatusOK, contentW.Code, "content body: %s", contentW.Body.String())
	var contentResp struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Size    int    `json:"size"`
	}
	require.NoError(t, json.Unmarshal(contentW.Body.Bytes(), &contentResp))
	assert.Equal(t, agentFilePath, contentResp.Path)
	assert.Equal(t, agentFileBody, contentResp.Content)
	assert.Equal(t, len(agentFileBody), contentResp.Size)
}

// TestFileOps_AnonymousUpload_401 verifies the workspace routes are gated by
// AuthMiddleware.
func TestFileOps_AnonymousUpload_401(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// mustMarshalWriteArgs builds the JSON arguments for xizhi_write_file.
func mustMarshalWriteArgs(t *testing.T, path, content string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"path": path, "content": content})
	require.NoError(t, err)
	return string(b)
}
