package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

// newDeleteSvc wires a SessionService with a drain hook, sharing one order
// log across the fakes so tests can assert the drain → purge → cache-clear
// sequence.
func newDeleteSvc(m *fakeMySQLStore, r *fakeRedisStore, d *fakeDrain) *SessionService {
	order := &[]string{}
	m.order = order
	r.order = order
	d.order = order
	return NewSessionService(SessionDeps{
		MySQL:             m,
		Redis:             r,
		FS:                &fakeFSStore{},
		DrainMessageQueue: d.drain,
	})
}

// TestDeleteSession_Success_ThreeStepOrder verifies the happy path and its
// strict ordering: ① drain the write-behind queue → ② MySQL archive+purge →
// ③ clear the Redis cache keys (database before cache).
func TestDeleteSession_Success_ThreeStepOrder(t *testing.T) {
	const (
		userID    = "u-del-1"
		sessionID = "s-del-1"
	)
	m := &fakeMySQLStore{getSessionByIDFound: &model.Session{SessionID: sessionID, UserID: userID}}
	r := &fakeRedisStore{}
	d := &fakeDrain{}
	svc := newDeleteSvc(m, r, d)

	require.NoError(t, svc.DeleteSession(context.Background(), userID, sessionID))

	require.Equal(t, 1, m.deleteSessionCalls)
	assert.Equal(t, sessionID, m.deleteSessionArg)
	require.Equal(t, 1, d.calls, "pre-delete drain must run exactly once")
	require.Equal(t, 1, r.clearCalls, "msgs:{sid} cache key must be cleared")
	require.Equal(t, 1, r.delSessCalls, "session:{sid} cache key must be cleared")

	require.Equal(t, []string{"drain", "mysql.delete", "redis.clear_msgs", "redis.del_session"}, *r.order,
		"delete must run drain → purge → cache clear in order")
}

// TestDeleteSession_DrainFailure_AbortsDelete verifies a failed drain aborts
// before any archive happens — archiving a session whose unflushed messages
// are still in the ingest queue would silently truncate the mirror tables.
func TestDeleteSession_DrainFailure_AbortsDelete(t *testing.T) {
	const (
		userID    = "u-del-drain"
		sessionID = "s-del-drain"
	)
	m := &fakeMySQLStore{getSessionByIDFound: &model.Session{SessionID: sessionID, UserID: userID}}
	r := &fakeRedisStore{}
	d := &fakeDrain{err: errFake}
	svc := newDeleteSvc(m, r, d)

	err := svc.DeleteSession(context.Background(), userID, sessionID)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrSessionNotFound)
	assert.Equal(t, 1, d.calls)
	assert.Equal(t, 0, m.deleteSessionCalls, "purge must not run when the drain fails")
	assert.Equal(t, 0, r.clearCalls, "cache must not be cleared when the drain fails")
}

// TestDeleteSession_NilDrainHook_Proceeds verifies legacy wiring (no drain
// hook) still deletes — the drain is skipped, not fatal.
func TestDeleteSession_NilDrainHook_Proceeds(t *testing.T) {
	const (
		userID    = "u-del-nil"
		sessionID = "s-del-nil"
	)
	m := &fakeMySQLStore{getSessionByIDFound: &model.Session{SessionID: sessionID, UserID: userID}}
	r := &fakeRedisStore{}
	svc := NewSessionService(SessionDeps{MySQL: m, Redis: r, FS: &fakeFSStore{}})

	require.NoError(t, svc.DeleteSession(context.Background(), userID, sessionID))
	require.Equal(t, 1, m.deleteSessionCalls)
	require.Equal(t, 1, r.clearCalls)
}

// TestDeleteSession_SessionMissing_NotFound verifies a non-existent session
// returns ErrSessionNotFound and drains/purges/clears nothing.
func TestDeleteSession_SessionMissing_NotFound(t *testing.T) {
	const (
		userID    = "u-del-2"
		sessionID = "s-del-2"
	)
	m := &fakeMySQLStore{getSessionByIDFound: nil} // session does not exist
	r := &fakeRedisStore{}
	d := &fakeDrain{}
	svc := newDeleteSvc(m, r, d)

	err := svc.DeleteSession(context.Background(), userID, sessionID)
	require.ErrorIs(t, err, ErrSessionNotFound)
	assert.Equal(t, 0, m.deleteSessionCalls, "MySQL must not be purged on not-found")
	assert.Equal(t, 0, d.calls, "not-found must not drain")
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
	d := &fakeDrain{}
	svc := newDeleteSvc(m, r, d)

	err := svc.DeleteSession(context.Background(), userID, sessionID)
	require.ErrorIs(t, err, ErrSessionNotFound)
	assert.Equal(t, 0, m.deleteSessionCalls, "MySQL must not be purged for non-owner")
	assert.Equal(t, 0, d.calls)
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
	d := &fakeDrain{}
	svc := newDeleteSvc(m, r, d)

	err := svc.DeleteSession(context.Background(), userID, sessionID)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrSessionNotFound)
	assert.Equal(t, 0, m.deleteSessionCalls)
	assert.Equal(t, 0, d.calls)
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
	d := &fakeDrain{}
	svc := newDeleteSvc(m, r, d)

	err := svc.DeleteSession(context.Background(), userID, sessionID)
	require.Error(t, err)
	assert.Equal(t, 1, m.deleteSessionCalls)
	assert.Equal(t, 1, d.calls, "drain runs before the purge attempt")
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
	d := &fakeDrain{}
	svc := newDeleteSvc(m, r, d)

	require.NoError(t, svc.DeleteSession(context.Background(), userID, sessionID),
		"cache clear failure must not surface from DeleteSession")
	require.Equal(t, 1, m.deleteSessionCalls)
	require.Equal(t, 1, r.clearCalls, "msgs cache clear must still be attempted")
	require.Equal(t, 1, r.delSessCalls, "session cache clear must still be attempted")
}
