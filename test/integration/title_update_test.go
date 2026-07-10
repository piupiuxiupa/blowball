package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestManualTitleUpdate_Success verifies a user can set a session title and the
// response echoes the sanitized title plus a refreshed update_time.
func TestManualTitleUpdate_Success(t *testing.T) {
	llm := newScriptedLLMClient()
	env := newTestEnv(t, llm)
	token := authToken(t, defaultUserID)

	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/sessions/"+defaultSessionID,
		strings.NewReader(`{"title":"  Manual Title  "}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		SessionID  string `json:"session_id"`
		Title      string `json:"title"`
		UpdateTime string `json:"update_time"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, defaultSessionID, resp.SessionID)
	assert.Equal(t, "Manual Title", resp.Title)
	_, err := time.Parse(time.RFC3339, resp.UpdateTime)
	require.NoError(t, err)

	title := env.mysqlFake.titles[defaultSessionID]
	assert.Equal(t, "Manual Title", title.Title)
	assert.True(t, title.IsManual, "manual title must be flagged")
}

// TestManualTitleUpdate_NotOverwrittenByAI verifies that after a user sets a
// manual title, the asynchronous AI title generation on the first turn does not
// overwrite it.
func TestManualTitleUpdate_NotOverwrittenByAI(t *testing.T) {
	// The scripted LLM has no queued responses, so AI title generation would
	// normally fall back to the first 20 chars of the user message. With a
	// manual title, the generation should be skipped entirely.
	llm := newScriptedLLMClient(
		scriptedLLMResponse{content: "Assistant reply"},
	)
	env := newTestEnv(t, llm)
	token := authToken(t, defaultUserID)

	// Set manual title first.
	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/sessions/"+defaultSessionID,
		strings.NewReader(`{"title":"Pinned Title"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Send the first message to trigger title generation.
	w = env.postMessage(`{"content":"this is a very long first message"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Wait for the fire-and-forget title goroutine to complete (or not).
	time.Sleep(100 * time.Millisecond)

	title := env.mysqlFake.titles[defaultSessionID]
	assert.Equal(t, "Pinned Title", title.Title, "manual title must not be overwritten")
	assert.True(t, title.IsManual)
}

// TestManualTitleUpdate_SessionNotFound_404 verifies updating another user's
// session returns 404.
func TestManualTitleUpdate_SessionNotFound_404(t *testing.T) {
	llm := newScriptedLLMClient()
	env := newTestEnv(t, llm)
	token := authToken(t, "other-user")

	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/sessions/"+defaultSessionID,
		strings.NewReader(`{"title":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
}

// TestManualTitleUpdate_EmptyTitle_400 verifies whitespace-only titles are
// rejected.
func TestManualTitleUpdate_EmptyTitle_400(t *testing.T) {
	llm := newScriptedLLMClient()
	env := newTestEnv(t, llm)
	token := authToken(t, defaultUserID)

	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/sessions/"+defaultSessionID,
		strings.NewReader(`{"title":"   "}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
}
