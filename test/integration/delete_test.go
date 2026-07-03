package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

// TestDeleteSession_EndToEnd seeds all three persistence tiers for a session,
// deletes it through the HTTP API, and verifies: the live rows are purged, the
// archive mirrors are populated, the warm-tier FS file is removed, the Redis
// cache is left untouched (TTL-only), and a subsequent read returns 404.
func TestDeleteSession_EndToEnd(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())
	ctx := context.Background()
	sessionID := defaultSessionID
	userID := defaultUserID

	// Seed the MySQL tier: a title plus two messages (the session itself is
	// already seeded by newTestEnv).
	require.NoError(t, env.mysqlFake.UpsertTitle(ctx, model.Title{
		SessionID: sessionID, Title: "Chat", TraceID: "seed-trace",
	}))
	_, err := env.mysqlFake.AppendMessages(ctx, []model.Message{
		{SessionID: sessionID, Agent: model.AgentUser, MsgIndex: 0, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "hi", TraceID: "seed-trace"},
		{SessionID: sessionID, Agent: stream.AgentConfucius, MsgIndex: 1, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "hello", TraceID: "seed-trace"},
	})
	require.NoError(t, err)

	// Seed the warm FS tier with a session JSON file.
	fsPath := filepath.Join(env.dataDir, userID, "sessions", sessionID+".json")
	require.NoError(t, os.MkdirAll(filepath.Dir(fsPath), 0o755))
	require.NoError(t, os.WriteFile(fsPath, []byte(`{"session_id":"`+sessionID+`"}`), 0o644))

	// Seed the hot Redis tier so we can later assert it is left in place.
	require.NoError(t, env.redisSvc.SetMessages(ctx, sessionID, [][]byte{[]byte("raw")}))
	require.True(t, env.miniRedis.Exists("msgs:"+sessionID), "precondition: redis key seeded")

	// Delete the session through the real HTTP stack.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+sessionID, nil)
	req.Header.Set("Authorization", "Bearer "+authToken(t, userID))
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	// Live MySQL rows are gone; archive mirrors captured them.
	env.mysqlFake.mu.Lock()
	_, liveSession := env.mysqlFake.sessions[sessionID]
	assert.False(t, liveSession, "live session must be purged")
	_, liveTitle := env.mysqlFake.titles[sessionID]
	assert.False(t, liveTitle, "live title must be purged")
	assert.Empty(t, env.mysqlFake.messages[sessionID], "live messages must be purged")

	archived, ok := env.mysqlFake.deletedSessions[sessionID]
	require.True(t, ok, "session must be archived to *_deleted")
	assert.Equal(t, userID, archived.UserID)
	assert.NotEmpty(t, env.mysqlFake.deletionIDs[sessionID], "archive must record a deletion_id")
	require.Len(t, env.mysqlFake.deletedMessages[sessionID], 2, "both messages must be archived")
	env.mysqlFake.mu.Unlock()

	// Warm-tier FS file is removed.
	_, statErr := os.Stat(fsPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "FS session JSON must be removed")

	// Redis is intentionally not cleared; the key survives until TTL.
	assert.True(t, env.miniRedis.Exists("msgs:"+sessionID),
		"redis cache must NOT be cleared on delete (TTL-only)")

	// A subsequent read returns 404: the ownership lookup misses the purged
	// session before Redis/FS are ever consulted.
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessionID+"/messages", nil)
	getReq.Header.Set("Authorization", "Bearer "+authToken(t, userID))
	getW := httptest.NewRecorder()
	env.engine.ServeHTTP(getW, getReq)
	require.Equal(t, http.StatusNotFound, getW.Code, "body: %s", getW.Body.String())
}

// TestDeleteSession_NonOwner_404 verifies a user cannot delete another user's
// session, and that no tiers are touched.
func TestDeleteSession_NonOwner_404(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())
	sessionID := defaultSessionID

	// Authenticate as a different user than the session owner.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+sessionID, nil)
	req.Header.Set("Authorization", "Bearer "+authToken(t, "intruder"))
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())

	// The session survives untouched.
	env.mysqlFake.mu.Lock()
	_, ok := env.mysqlFake.sessions[sessionID]
	assert.True(t, ok, "non-owner delete must not purge the session")
	assert.Empty(t, env.mysqlFake.deletedSessions, "non-owner delete must not archive anything")
	env.mysqlFake.mu.Unlock()
}

// TestDeleteSession_MissingSession_404 verifies deleting a session that never
// existed returns 404 (and never reaches the store purge).
func TestDeleteSession_MissingSession_404(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())
	missing := "cccccccc-0000-7000-8000-0000000000ff"

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+missing, nil)
	req.Header.Set("Authorization", "Bearer "+authToken(t, ""))
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())

	env.mysqlFake.mu.Lock()
	assert.Empty(t, env.mysqlFake.deletedSessions, "missing-session delete must not archive anything")
	env.mysqlFake.mu.Unlock()
}
