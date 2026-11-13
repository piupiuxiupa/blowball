package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/model"
)

type ensureSessionMySQL struct {
	*fakeMySQLStore
}

func (f *ensureSessionMySQL) CreateSession(ctx context.Context, sess model.Session) error {
	err := f.fakeMySQLStore.CreateSession(ctx, sess)
	f.mu.Lock()
	f.getSessionByIDFound = &sess
	f.mu.Unlock()
	return err
}

func TestEnsureSessionWithIDIsIdempotent(t *testing.T) {
	mysql := &ensureSessionMySQL{fakeMySQLStore: &fakeMySQLStore{}}
	svc := NewSessionService(SessionDeps{MySQL: mysql, Redis: &fakeRedisStore{}, FS: &fakeFSStore{}})
	ctx := context.Background()
	first, err := svc.EnsureSessionWithID(ctx, "user-1", "reserved-session", "run-1")
	require.NoError(t, err)
	require.Equal(t, "reserved-session", first.SessionID)
	second, err := svc.EnsureSessionWithID(ctx, "user-1", "reserved-session", "run-1")
	require.NoError(t, err)
	require.Equal(t, first.SessionID, second.SessionID)
	require.Equal(t, 1, mysql.createSessionCalls, "repeated preparation creates only one row")
}

func TestEnsureSessionWithIDRejectsForeignAndHandlesInsertRace(t *testing.T) {
	mysql := &ensureSessionMySQL{fakeMySQLStore: &fakeMySQLStore{getSessionByIDFound: &model.Session{SessionID: "reserved", UserID: "user-2"}}}
	svc := NewSessionService(SessionDeps{MySQL: mysql, Redis: &fakeRedisStore{}, FS: &fakeFSStore{}})
	_, err := svc.EnsureSessionWithID(context.Background(), "user-1", "reserved", "run-1")
	require.Error(t, err)
	require.Zero(t, mysql.createSessionCalls)

	mysql.getSessionByIDFound = nil
	mysql.createSessionErr = errors.New("duplicate entry")
	created, err := svc.EnsureSessionWithID(context.Background(), "user-1", "reserved", "run-1")
	require.NoError(t, err, "a lost insert race succeeds after the same owner row is readable")
	require.Equal(t, "user-1", created.UserID)
}
