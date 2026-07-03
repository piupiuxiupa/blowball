package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

// TestDeleteSession_Success verifies the happy path: an owned session is
// archived+purged via the MySQL store and the warm-tier FS file is removed.
func TestDeleteSession_Success(t *testing.T) {
	const (
		userID    = "u-del-1"
		sessionID = "s-del-1"
	)
	m := &fakeMySQLStore{getSessionByIDFound: &model.Session{SessionID: sessionID, UserID: userID}}
	r := &fakeRedisStore{}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	require.NoError(t, svc.DeleteSession(context.Background(), userID, sessionID))

	require.Equal(t, 1, m.deleteSessionCalls)
	assert.Equal(t, sessionID, m.deleteSessionArg)
	require.Equal(t, 1, f.deleteCalls, "FS session file must be removed on success")
}

// TestDeleteSession_SessionMissing_NotFound verifies a non-existent session
// returns ErrSessionNotFound and touches neither MySQL nor FS.
func TestDeleteSession_SessionMissing_NotFound(t *testing.T) {
	const (
		userID    = "u-del-2"
		sessionID = "s-del-2"
	)
	m := &fakeMySQLStore{getSessionByIDFound: nil} // session does not exist
	r := &fakeRedisStore{}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	err := svc.DeleteSession(context.Background(), userID, sessionID)
	require.ErrorIs(t, err, ErrSessionNotFound)
	assert.Equal(t, 0, m.deleteSessionCalls, "MySQL must not be purged on not-found")
	assert.Equal(t, 0, f.deleteCalls, "FS must not be touched on not-found")
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
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	err := svc.DeleteSession(context.Background(), userID, sessionID)
	require.ErrorIs(t, err, ErrSessionNotFound)
	assert.Equal(t, 0, m.deleteSessionCalls, "MySQL must not be purged for non-owner")
	assert.Equal(t, 0, f.deleteCalls, "FS must not be touched for non-owner")
}

// TestDeleteSession_LookupError_ReturnsError verifies a store error during the
// ownership lookup is surfaced (not mapped to not-found) and nothing is purged.
func TestDeleteSession_LookupError_ReturnsError(t *testing.T) {
	const (
		userID    = "u-del-4"
		sessionID = "s-del-4"
	)
	m := &fakeMySQLStore{getSessionIDErr: errFake}
	r := &fakeRedisStore{}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	err := svc.DeleteSession(context.Background(), userID, sessionID)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrSessionNotFound)
	assert.Equal(t, 0, m.deleteSessionCalls)
	assert.Equal(t, 0, f.deleteCalls)
}

// TestDeleteSession_PurgeError_ReturnsError verifies a MySQL purge failure is
// surfaced and the FS cleanup is never reached.
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
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	err := svc.DeleteSession(context.Background(), userID, sessionID)
	require.Error(t, err)
	assert.Equal(t, 1, m.deleteSessionCalls)
	assert.Equal(t, 0, f.deleteCalls, "FS must not be reached when MySQL purge fails")
}

// TestDeleteSession_FSFailure_BestEffort verifies a warm-tier cleanup failure is
// logged but does NOT fail the request — the MySQL purge already succeeded, so
// the session is gone from the source of truth regardless of the stale FS file.
func TestDeleteSession_FSFailure_BestEffort(t *testing.T) {
	const (
		userID    = "u-del-6"
		sessionID = "s-del-6"
	)
	m := &fakeMySQLStore{getSessionByIDFound: &model.Session{SessionID: sessionID, UserID: userID}}
	r := &fakeRedisStore{}
	f := &fakeFSStore{deleteErr: errors.New("disk full")}
	svc := NewSessionService(newDeps(m, r, f))

	require.NoError(t, svc.DeleteSession(context.Background(), userID, sessionID),
		"FS cleanup failure must not surface from DeleteSession")
	require.Equal(t, 1, m.deleteSessionCalls)
	require.Equal(t, 1, f.deleteCalls, "FS DeleteSession must still be attempted")
}
