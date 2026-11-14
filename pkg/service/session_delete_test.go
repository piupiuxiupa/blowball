package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/model"
)

// newDeleteSvc wires a SessionService, sharing one order log across the
// fakes so tests can assert the purge → cache-clear sequence.
func newDeleteSvc(m *fakeMySQLStore, r *fakeRedisStore) *SessionService {
	order := &[]string{}
	m.order = order
	r.order = order
	return NewSessionService(SessionDeps{
		MySQL: m,
		Redis: r,
		FS:    &fakeFSStore{},
	})
}

// TestDeleteSession_RemovesAttachmentDir verifies step ④
// (add-image-attachments): after the archive transaction and cache clear,
// the session's attachments/{sessionID} directory is removed wholesale; a
// removal failure is logged only and never fails the delete.
func TestDeleteSession_RemovesAttachmentDir(t *testing.T) {
	const (
		userID    = "u-del-att"
		sessionID = "s-del-att"
	)
	root := t.TempDir()
	dir := filepath.Join(root, userID, "attachments", sessionID)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "img.png"), []byte("png"), 0o644))

	m := &fakeMySQLStore{getSessionByIDFound: &model.Session{SessionID: sessionID, UserID: userID}}
	r := &fakeRedisStore{}
	svc := newDeleteSvc(m, r).WithAttachmentDir(func(uid, sid string) string {
		return filepath.Join(root, uid, "attachments", sid)
	})

	require.NoError(t, svc.DeleteSession(context.Background(), userID, sessionID))
	_, err := os.Stat(dir)
	require.True(t, os.IsNotExist(err), "attachment dir must be removed with the session")
}

// TestDeleteSession_AttachmentDirRemovalFailureDoesNotFailDelete: RemoveAll
// errors (e.g. a path under a non-directory) are logged, not surfaced.
func TestDeleteSession_AttachmentDirRemovalFailureDoesNotFailDelete(t *testing.T) {
	const (
		userID    = "u-del-att-err"
		sessionID = "s-del-att-err"
	)
	// A path beneath a regular file makes RemoveAll fail with ENOTDIR.
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o644))

	m := &fakeMySQLStore{getSessionByIDFound: &model.Session{SessionID: sessionID, UserID: userID}}
	r := &fakeRedisStore{}
	svc := newDeleteSvc(m, r).WithAttachmentDir(func(string, string) string {
		return filepath.Join(blocker, "child")
	})

	require.NoError(t, svc.DeleteSession(context.Background(), userID, sessionID))
	require.Equal(t, 1, m.deleteSessionCalls, "the archive+purge already committed")
}

// TestDeleteSession_NoAttachmentDirWired: the unwired (nil) resolver keeps
// the pre-attachment delete behavior exactly.
func TestDeleteSession_NoAttachmentDirWired(t *testing.T) {
	const (
		userID    = "u-del-att-nil"
		sessionID = "s-del-att-nil"
	)
	m := &fakeMySQLStore{getSessionByIDFound: &model.Session{SessionID: sessionID, UserID: userID}}
	r := &fakeRedisStore{}
	svc := newDeleteSvc(m, r)

	require.NoError(t, svc.DeleteSession(context.Background(), userID, sessionID))
	require.Equal(t, 1, m.deleteSessionCalls)
}

// TestDeleteSession_Success_PurgeThenCacheClear verifies the happy path and
// its strict ordering: ① MySQL archive+purge → ② clear the Redis cache keys
// (database before cache).
func TestDeleteSession_Success_PurgeThenCacheClear(t *testing.T) {
	const (
		userID    = "u-del-1"
		sessionID = "s-del-1"
	)
	m := &fakeMySQLStore{getSessionByIDFound: &model.Session{SessionID: sessionID, UserID: userID}}
	r := &fakeRedisStore{}
	svc := newDeleteSvc(m, r)

	require.NoError(t, svc.DeleteSession(context.Background(), userID, sessionID))

	require.Equal(t, 1, m.deleteSessionCalls)
	assert.Equal(t, sessionID, m.deleteSessionArg)
	require.Equal(t, 1, r.clearCalls, "msgs:{sid} cache key must be cleared")
	require.Equal(t, 1, r.delSessCalls, "session:{sid} cache key must be cleared")

	require.Equal(t, []string{"mysql.delete", "redis.clear_msgs", "redis.del_session"}, *r.order,
		"delete must run purge → cache clear in order")
}

// TestDeleteSession_SessionMissing_NotFound verifies a non-existent session
// returns ErrSessionNotFound and purges/clears nothing.
func TestDeleteSession_SessionMissing_NotFound(t *testing.T) {
	const (
		userID    = "u-del-2"
		sessionID = "s-del-2"
	)
	m := &fakeMySQLStore{getSessionByIDFound: nil} // session does not exist
	r := &fakeRedisStore{}
	svc := newDeleteSvc(m, r)

	err := svc.DeleteSession(context.Background(), userID, sessionID)
	require.ErrorIs(t, err, ErrSessionNotFound)
	assert.Equal(t, 0, m.deleteSessionCalls, "MySQL must not be purged on not-found")
	assert.Equal(t, 0, r.clearCalls, "not-found must not clear the cache")
}

// TestDeleteSession_WrongOwner_NotFound verifies a session owned by another
// user is reported as not-found (no existence leak) and is not deleted.
func TestDeleteSession_WrongOwner_NotFound(t *testing.T) {
	const (
		userID    = "u-del-3"
		sessionID = "s-del-3"
	)
	m := &fakeMySQLStore{getSessionByIDFound: &model.Session{SessionID: sessionID, UserID: "someone-else"}}
	r := &fakeRedisStore{}
	svc := newDeleteSvc(m, r)

	err := svc.DeleteSession(context.Background(), userID, sessionID)
	require.ErrorIs(t, err, ErrSessionNotFound)
	assert.Equal(t, 0, m.deleteSessionCalls, "MySQL must not be purged for non-owner")
}

// TestDeleteSession_LookupError_ReturnsError verifies a store error during the
// ownership lookup is surfaced (not mapped to not-found) and nothing else runs.
func TestDeleteSession_LookupError_ReturnsError(t *testing.T) {
	const (
		userID    = "u-del-4"
		sessionID = "s-del-4"
	)
	m := &fakeMySQLStore{getSessionIDErr: errFake}
	r := &fakeRedisStore{}
	svc := newDeleteSvc(m, r)

	err := svc.DeleteSession(context.Background(), userID, sessionID)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrSessionNotFound)
	assert.Equal(t, 0, m.deleteSessionCalls)
	assert.Equal(t, 0, r.clearCalls)
}

// TestDeleteSession_PurgeError_ReturnsError verifies a MySQL purge failure is
// surfaced and the cache clear is never reached (the source of truth still
// holds the session; clearing the cache first would be wrong).
func TestDeleteSession_PurgeError_ReturnsError(t *testing.T) {
	const (
		userID    = "u-del-5"
		sessionID = "s-del-5"
	)
	m := &fakeMySQLStore{
		getSessionByIDFound: &model.Session{SessionID: sessionID, UserID: userID},
		deleteSessionErr:    errFake,
	}
	r := &fakeRedisStore{}
	svc := newDeleteSvc(m, r)

	err := svc.DeleteSession(context.Background(), userID, sessionID)
	require.Error(t, err)
	assert.Equal(t, 1, m.deleteSessionCalls)
	assert.Equal(t, 0, r.clearCalls, "cache must not be cleared when the purge fails")
}

// TestDeleteSession_CacheClearFailure_BestEffort verifies a Redis clear
// failure after a successful purge does not fail the delete — the database
// rows are already gone and the stale key ages out on TTL.
func TestDeleteSession_CacheClearFailure_BestEffort(t *testing.T) {
	const (
		userID    = "u-del-6"
		sessionID = "s-del-6"
	)
	m := &fakeMySQLStore{getSessionByIDFound: &model.Session{SessionID: sessionID, UserID: userID}}
	r := &fakeRedisStore{clearErr: errFake}
	svc := newDeleteSvc(m, r)

	require.NoError(t, svc.DeleteSession(context.Background(), userID, sessionID),
		"cache clear failure must not surface from DeleteSession")
	require.Equal(t, 1, m.deleteSessionCalls)
	require.Equal(t, 1, r.clearCalls, "msgs cache clear must still be attempted")
	require.Equal(t, 1, r.delSessCalls, "session cache clear must still be attempted")
}
