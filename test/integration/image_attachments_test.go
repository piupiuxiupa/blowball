package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
)

const visionChatModel = "vision-chat-model"

var chatPNG = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, []byte("integration-png")...)

func imageAttachmentsTestConfig() *config.Config {
	return &config.Config{
		OpenAI: config.OpenAIConfig{
			APIKey: "test",
			Models: append(testCatalog(), config.ModelCatalogEntry{
				Name:                visionChatModel,
				MaxContextTokens:    128000,
				MaxCompletionTokens: 512,
				Thinking:            false,
				Vision:              true,
			}),
		},
		JWT:    config.JWTConfig{Secret: integrationTestSecret, Expire: "1h"},
		Agents: agentConfig(),
	}
}

func uploadAttachment(t *testing.T, env *testEnv, token string) string {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "screenshot.png")
	require.NoError(t, err)
	_, err = fw.Write(chatPNG)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/"+defaultSessionID+"/attachments", &body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "upload body: %s", w.Body.String())

	var up struct {
		AttachmentID string `json:"attachment_id"`
		MIME         string `json:"mime"`
		Size         int64  `json:"size"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &up))
	assert.Equal(t, "image/png", up.MIME)
	assert.Equal(t, int64(len(chatPNG)), up.Size)
	return up.AttachmentID
}

// TestImageAttachments_EndToEnd drives the full capability through the real
// handler + orchestrator + persistence stack: upload → send on the vision
// model → the LLM request carries the inline image → the next turn replays
// the row as a deterministic placeholder → the history API returns the
// structured references → the query-token content endpoint serves immutable
// bytes → deleting the session removes registry rows and the directory.
func TestImageAttachments_EndToEnd(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{ // Turn 1: plain answer.
			tokens: []string{"the image shows a build error"}, content: "the image shows a build error", finishReason: "stop",
			usage: agent.Usage{PromptTokens: 30, CompletionTokens: 5, TotalTokens: 35},
		},
		scriptedLLMResponse{ // Turn 2: answer over placeholder history.
			tokens: []string{"as I said, a build error"}, content: "as I said, a build error", finishReason: "stop",
			usage: agent.Usage{PromptTokens: 40, CompletionTokens: 5, TotalTokens: 45},
		},
		// Title generation shares the scripted client (first-turn cadence).
		scriptedLLMResponse{content: "Build error chat", finishReason: "stop",
			usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}},
	)
	env := newTestEnvWithConfig(t, llm, imageAttachmentsTestConfig())
	token := authToken(t, defaultUserID)

	// 1) Upload.
	attID := uploadAttachment(t, env, token)
	_, err := os.Stat(filepath.Join(env.fsSvc.UserAttachmentDir(defaultUserID, defaultSessionID), attID+".png"))
	require.NoError(t, err, "bytes must land on disk at the id-derived path")

	// 2) Send with attachments on the vision model.
	w := env.postMessage(fmt.Sprintf(`{"content":"what is in this screenshot?","model":%q,"attachment_ids":[%q]}`,
		visionChatModel, attID), token)
	require.Equal(t, http.StatusOK, w.Code, "sse body: %s", w.Body.String())
	types, _ := parseSSEBody(t, w.Body.String())
	requireEventPresent(t, types, stream.EventDone)
	env.waitForPersistedTurn(t, defaultSessionID, 1)

	// 3) The turn's LLM request inlined the image on the live user message.
	// (The scripted client also sees the concurrent title-generation call —
	// find the chat request by the user question, not by call order.)
	req1 := findLLMRequest(t, llm, func(m agent.Message) bool {
		return m.Role == "user" && strings.HasPrefix(m.Content, "what is in this screenshot?")
	})
	last := req1.Messages[len(req1.Messages)-1]
	require.Equal(t, "user", last.Role)
	require.Len(t, last.Images, 1)
	assert.Equal(t, attID, last.Images[0].AttachmentID)
	assert.Equal(t, chatPNG, last.Images[0].Data)

	// 4) Turn 2: the same row replays as the deterministic placeholder.
	w = env.postMessage(`{"content":"summarize again","model":"`+visionChatModel+`"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "sse body: %s", w.Body.String())
	types2, _ := parseSSEBody(t, w.Body.String())
	requireEventPresent(t, types2, stream.EventDone)
	env.waitForPersistedTurn(t, defaultSessionID, 3)

	req2 := findLLMRequest(t, llm, func(m agent.Message) bool {
		return m.Role == "user" && strings.HasPrefix(m.Content, "summarize again")
	})
	foundPlaceholder := false
	for _, m := range req2.Messages {
		assert.Empty(t, m.Images, "history must never re-send image bytes")
		if m.Role == "user" && strings.Contains(m.Content, "[图片: screenshot.png]") {
			foundPlaceholder = true
		}
	}
	assert.True(t, foundPlaceholder, "turn-2 history must render the placeholder, got %+v", req2.Messages)

	// 5) History API returns the structured references.
	hist := env.getSessionMessages(defaultSessionID, "", token)
	require.Equal(t, http.StatusOK, hist.Code)
	var histBody struct {
		Messages []struct {
			Role        string `json:"role"`
			Attachments []struct {
				ID       string `json:"id"`
				Filename string `json:"filename"`
				MIME     string `json:"mime"`
			} `json:"attachments"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(hist.Body.Bytes(), &histBody))
	require.NotEmpty(t, histBody.Messages)
	var refs []struct {
		ID       string `json:"id"`
		Filename string `json:"filename"`
		MIME     string `json:"mime"`
	}
	for _, m := range histBody.Messages {
		if m.Role == "user" && len(m.Attachments) > 0 {
			refs = m.Attachments
		}
	}
	require.Len(t, refs, 1)
	assert.Equal(t, attID, refs[0].ID)
	assert.Equal(t, "screenshot.png", refs[0].Filename)

	// 6) Content endpoint: query-token auth, immutable cache, 304.
	contentURL := "/api/v1/sessions/" + defaultSessionID + "/attachments/" + attID + "/content?token=" + token
	req := httptest.NewRequest(http.MethodGet, contentURL, nil)
	cw := httptest.NewRecorder()
	env.engine.ServeHTTP(cw, req)
	require.Equal(t, http.StatusOK, cw.Code, "content body: %s", cw.Body.String())
	assert.Equal(t, "image/png", cw.Header().Get("Content-Type"))
	assert.Equal(t, `"`+attID+`"`, cw.Header().Get("ETag"))
	assert.Equal(t, "private, max-age=31536000, immutable", cw.Header().Get("Cache-Control"))
	assert.Equal(t, chatPNG, cw.Body.Bytes())

	req = httptest.NewRequest(http.MethodGet, contentURL, nil)
	req.Header.Set("If-None-Match", `"`+attID+`"`)
	nw := httptest.NewRecorder()
	env.engine.ServeHTTP(nw, req)
	require.Equal(t, http.StatusNotModified, nw.Code)

	// 7) Session delete archives the registry row and removes the directory.
	attDir := env.fsSvc.UserAttachmentDir(defaultUserID, defaultSessionID)
	del := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+defaultSessionID, nil)
	del.Header.Set("Authorization", "Bearer "+token)
	dw := httptest.NewRecorder()
	env.engine.ServeHTTP(dw, del)
	require.Equal(t, http.StatusNoContent, dw.Code)
	_, err = os.Stat(attDir)
	require.True(t, os.IsNotExist(err), "attachment dir must be removed with the session")

	env.mysqlFake.mu.Lock()
	_, liveRow := env.mysqlFake.attachments[attID]
	archivedRow, archived := env.mysqlFake.deletedAttachments[attID]
	env.mysqlFake.mu.Unlock()
	assert.False(t, liveRow, "live registry row purged")
	require.True(t, archived, "registry row archived")
	require.NotNil(t, archivedRow.SessionID)
	assert.Equal(t, defaultSessionID, *archivedRow.SessionID)
}

// findLLMRequest returns the first recorded LLM request containing a message
// matching pred (title generation shares the scripted client, so call order
// is not a stable discriminator).
func findLLMRequest(t *testing.T, llm *scriptedLLMClient, pred func(agent.Message) bool) agent.LLMRequest {
	t.Helper()
	for _, req := range llm.requests() {
		for _, m := range req.Messages {
			if pred(m) {
				return req
			}
		}
	}
	t.Fatalf("no recorded LLM request matches the predicate among %d calls", len(llm.requests()))
	return agent.LLMRequest{}
}

// uploadStagedAttachment posts one image to the session-independent staged
// endpoint (add-staged-attachment-upload) and returns its attachment id.
func uploadStagedAttachment(t *testing.T, env *testEnv, token string) string {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "staged.png")
	require.NoError(t, err)
	_, err = fw.Write(chatPNG)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/attachments", &body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "staged upload body: %s", w.Body.String())

	var up struct {
		AttachmentID string `json:"attachment_id"`
		MIME         string `json:"mime"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &up))
	require.NotEmpty(t, up.AttachmentID)
	return up.AttachmentID
}

// TestImageAttachments_StagedEndToEnd drives the add-staged-attachment-upload
// flow through the real stack: upload WITHOUT a session → the staged row is
// invisible to the content endpoint → the first message of a (possibly new)
// session claims it → the file moves into the session directory and the LLM
// request carries the image inline → history and the content endpoint behave
// exactly like a session-scoped upload → deleting the session removes the
// claimed file but leaves OTHER staged uploads untouched.
func TestImageAttachments_StagedEndToEnd(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens: []string{"a staged screenshot"}, content: "a staged screenshot", finishReason: "stop",
			usage: agent.Usage{PromptTokens: 30, CompletionTokens: 5, TotalTokens: 35},
		},
		scriptedLLMResponse{content: "Staged chat", finishReason: "stop",
			usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}},
	)
	env := newTestEnvWithConfig(t, llm, imageAttachmentsTestConfig())
	token := authToken(t, defaultUserID)

	// 1) Staged upload: no session involved, file parked under _staging.
	attID := uploadStagedAttachment(t, env, token)
	stagedPath := filepath.Join(env.fsSvc.UserAttachmentStagingDir(defaultUserID), attID+".png")
	_, err := os.Stat(stagedPath)
	require.NoError(t, err, "staged bytes must land under the _staging sentinel")

	// A second staged upload stays behind after the session delete (step 6).
	keepID := uploadStagedAttachment(t, env, token)

	// 2) The content endpoint never serves a staged row.
	stagedContent := httptest.NewRequest(http.MethodGet,
		"/api/v1/sessions/"+defaultSessionID+"/attachments/"+attID+"/content?token="+token, nil)
	scw := httptest.NewRecorder()
	env.engine.ServeHTTP(scw, stagedContent)
	require.Equal(t, http.StatusNotFound, scw.Code)

	// 3) The first message claims the staged id: file moves into the session
	// directory and the model sees the image inline.
	w := env.postMessage(fmt.Sprintf(`{"content":"what is in this screenshot?","model":%q,"attachment_ids":[%q]}`,
		visionChatModel, attID), token)
	require.Equal(t, http.StatusOK, w.Code, "sse body: %s", w.Body.String())
	types, _ := parseSSEBody(t, w.Body.String())
	requireEventPresent(t, types, stream.EventDone)
	env.waitForPersistedTurn(t, defaultSessionID, 1)

	_, err = os.Stat(stagedPath)
	require.True(t, os.IsNotExist(err), "the claim must move the file out of _staging")
	boundPath := filepath.Join(env.fsSvc.UserAttachmentDir(defaultUserID, defaultSessionID), attID+".png")
	got, err := os.ReadFile(boundPath)
	require.NoError(t, err)
	assert.Equal(t, chatPNG, got)

	req1 := findLLMRequest(t, llm, func(m agent.Message) bool {
		return m.Role == "user" && strings.HasPrefix(m.Content, "what is in this screenshot?")
	})
	last := req1.Messages[len(req1.Messages)-1]
	require.Len(t, last.Images, 1)
	assert.Equal(t, attID, last.Images[0].AttachmentID)
	assert.Equal(t, chatPNG, last.Images[0].Data)

	// 4) History carries the structured reference.
	hist := env.getSessionMessages(defaultSessionID, "", token)
	require.Equal(t, http.StatusOK, hist.Code)
	assert.Contains(t, hist.Body.String(), attID)

	// 5) Once claimed, the content endpoint serves it like any bound upload.
	contentURL := "/api/v1/sessions/" + defaultSessionID + "/attachments/" + attID + "/content?token=" + token
	cw := httptest.NewRecorder()
	env.engine.ServeHTTP(cw, httptest.NewRequest(http.MethodGet, contentURL, nil))
	require.Equal(t, http.StatusOK, cw.Code)
	assert.Equal(t, chatPNG, cw.Body.Bytes())

	// 6) Deleting the session removes the claimed attachment but leaves the
	// still-staged one (and its file) alone.
	del := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+defaultSessionID, nil)
	del.Header.Set("Authorization", "Bearer "+token)
	dw := httptest.NewRecorder()
	env.engine.ServeHTTP(dw, del)
	require.Equal(t, http.StatusNoContent, dw.Code, "delete body: %s", dw.Body.String())

	_, err = os.Stat(env.fsSvc.UserAttachmentDir(defaultUserID, defaultSessionID))
	require.True(t, os.IsNotExist(err), "session delete must remove the claimed attachment dir")
	keepPath := filepath.Join(env.fsSvc.UserAttachmentStagingDir(defaultUserID), keepID+".png")
	keepBytes, err := os.ReadFile(keepPath)
	require.NoError(t, err, "session delete must not touch other staged uploads")
	assert.Equal(t, chatPNG, keepBytes)
}
