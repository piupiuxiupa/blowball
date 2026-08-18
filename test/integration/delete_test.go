package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

// TestDeleteSession_EndToEnd seeds the MySQL tier and the Redis cache for a
// session, deletes it through the HTTP API, and verifies the three-step
// ordering: the live rows are purged, the archive mirrors are populated, the
// Redis cache keys are proactively cleared (database before cache — a
// behavior change from the TTL-only policy), and a subsequent read returns
// 404.
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
		{SessionID: sessionID, Agent: model.AgentUser, MsgIndex: 0, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "hi", TraceID: "seed-trace", ClientMsgID: "seed-cmid-1"},
		{SessionID: sessionID, Agent: stream.AgentConfucius, MsgIndex: 1, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "hello", TraceID: "seed-trace", ClientMsgID: "seed-cmid-2"},
	})
	require.NoError(t, err)

	// Seed the hot Redis tier so we can later assert it is cleared.
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

	// Redis cache keys are proactively cleared after the database delete.
	assert.False(t, env.miniRedis.Exists("msgs:"+sessionID),
		"msgs cache key must be cleared on delete (db-first, then cache)")
	assert.False(t, env.miniRedis.Exists("session:"+sessionID),
		"session cache key must be cleared on delete")

	// A subsequent read returns 404: the ownership lookup misses the purged
	// session before the cache is ever consulted.
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessionID+"/messages", nil)
	getReq.Header.Set("Authorization", "Bearer "+authToken(t, userID))
	getW := httptest.NewRecorder()
	env.engine.ServeHTTP(getW, getReq)
	require.Equal(t, http.StatusNotFound, getW.Code, "body: %s", getW.Body.String())
}

// TestDeleteSession_ArchiveIncludesQueuedMessages verifies the pre-delete
// drain: messages that were dual-written into Redis but not yet flushed to
// MySQL are drained into the archive, so the *_deleted mirror captures every
// message the session ever produced despite the async write-behind window.
func TestDeleteSession_ArchiveIncludesQueuedMessages(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())
	ctx := context.Background()
	sessionID := defaultSessionID
	userID := defaultUserID

	// Simulate a turn whose batch reached Redis (dual write) but has NOT been
	// flushed: push straight onto the ingest queue through the service API.
	msgs := []model.Message{
		{SessionID: sessionID, Agent: model.AgentUser, MsgIndex: 0, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "unflushed", TraceID: "t-1", ClientMsgID: "unflushed-cmid-1"},
		{SessionID: sessionID, Agent: stream.AgentConfucius, MsgIndex: 1, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "still-queued", TraceID: "t-1", ClientMsgID: "unflushed-cmid-2"},
	}
	raws := make([][]byte, 0, len(msgs))
	for _, m := range msgs {
		raws = append(raws, mustMarshal(t, m))
	}
	require.NoError(t, env.redisSvc.PushMessageBuffer(ctx, raws))
	require.Equal(t, int64(2), mustLenMessageBuffer(t, env), "precondition: queue holds the unflushed batch")

	// Delete through the real HTTP stack; the service drains first.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+sessionID, nil)
	req.Header.Set("Authorization", "Bearer "+authToken(t, userID))
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	env.mysqlFake.mu.Lock()
	require.Len(t, env.mysqlFake.deletedMessages[sessionID], 2,
		"the archive must include the queued-but-unflushed messages (drain ran first)")
	content := env.mysqlFake.deletedMessages[sessionID][0].Content
	env.mysqlFake.mu.Unlock()
	assert.Equal(t, "unflushed", content)
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

	require.Equal(t, http.StatusNotFound, w.Code)

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

	require.Equal(t, http.StatusNotFound, w.Code)

	env.mysqlFake.mu.Lock()
	assert.Empty(t, env.mysqlFake.deletedSessions, "missing-session delete must not archive anything")
	env.mysqlFake.mu.Unlock()
}
