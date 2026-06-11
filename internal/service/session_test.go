package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/trace"
	mysqlstore "github.com/lush/blowball/internal/store/mysql"
)

func TestSaveMessage_AllLayersSucceed(t *testing.T) {
	const (
		userID    = "u-1"
		sessionID = "s-1"
	)
	m := &fakeMySQLStore{appendMessageID: 42}
	r := &fakeRedisStore{}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	msg := sampleMessage(sessionID, "hello world")
	err := svc.SaveMessage(context.Background(), userID, msg)
	require.NoError(t, err)

	// Redis saw one append with the canonical JSON of the message.
	require.Equal(t, 1, r.appendCalls)
	var got model.Message
	require.NoError(t, json.Unmarshal(r.appendArg, &got))
	assert.Equal(t, msg, got)

	// FS saw exactly one write. The written payload must be a sessionFile whose
	// messages[] contains exactly one element equal to the original message.
	require.Equal(t, 1, f.writeCalls)
	var doc sessionFile
	require.NoError(t, json.Unmarshal(f.writeData, &doc))
	assert.Equal(t, sessionID, doc.SessionID)
	require.Len(t, doc.Messages, 1)
	var fsMsg model.Message
	require.NoError(t, json.Unmarshal(doc.Messages[0], &fsMsg))
	assert.Equal(t, msg, fsMsg)

	// MySQL saw exactly one append with the original message struct.
	require.Equal(t, 1, m.appendMessageCalls)
	assert.Equal(t, msg, m.appendMessageArg)
}

func TestSaveMessage_RedisFailure_DoesNotBlock(t *testing.T) {
	const (
		userID    = "u-2"
		sessionID = "s-2"
	)
	m := &fakeMySQLStore{}
	r := &fakeRedisStore{appendErr: errFake}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	err := svc.SaveMessage(context.Background(), userID, sampleMessage(sessionID, "x"))
	require.NoError(t, err, "redis failure must NOT surface to caller")

	require.Equal(t, 1, r.appendCalls)
	require.Equal(t, 1, f.writeCalls, "FS write must still happen")
	require.Equal(t, 1, m.appendMessageCalls, "MySQL write must still happen")
}

func TestSaveMessage_MysqlFailure_LoggedNotReturned(t *testing.T) {
	const (
		userID    = "u-3"
		sessionID = "s-3"
	)
	m := &fakeMySQLStore{appendMessageErr: errFake}
	r := &fakeRedisStore{}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	err := svc.SaveMessage(context.Background(), userID, sampleMessage(sessionID, "x"))
	require.NoError(t, err, "mysql failure must NOT surface to caller per design choice")

	require.Equal(t, 1, r.appendCalls, "redis still attempted")
	require.Equal(t, 1, f.writeCalls, "FS still attempted")
	require.Equal(t, 1, m.appendMessageCalls, "MySQL attempt counted even on failure")
}

func TestSaveMessage_FSFailure_Returned(t *testing.T) {
	const (
		userID    = "u-4"
		sessionID = "s-4"
	)
	m := &fakeMySQLStore{}
	r := &fakeRedisStore{}
	f := &fakeFSStore{readErr: errFake}
	svc := NewSessionService(newDeps(m, r, f))

	err := svc.SaveMessage(context.Background(), userID, sampleMessage(sessionID, "x"))
	require.Error(t, err)
}

func TestListSessions_WithTitle(t *testing.T) {
	const userID = "u-5"
	m := &fakeMySQLStore{
		listSessionsWithTitleRows: []mysqlstore.SessionWithTitle{
			{SessionID: "s-a", UserID: userID, Title: "Alpha", UpdateTime: time.Unix(2, 0).UTC()},
			{SessionID: "s-b", UserID: userID, Title: "", UpdateTime: time.Unix(1, 0).UTC()},
		},
	}
	r := &fakeRedisStore{}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	got, err := svc.ListSessions(context.Background(), userID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "s-a", got[0].SessionID)
	assert.Equal(t, "Alpha", got[0].Title)
	assert.Equal(t, "s-b", got[1].SessionID)
	assert.Equal(t, "", got[1].Title, "session without title must carry empty string")
}

func TestListSessions_OrderedDESC(t *testing.T) {
	const userID = "u-6"
	base := time.Unix(1_700_000_000, 0).UTC()
	m := &fakeMySQLStore{
		// Store layer returns rows already ordered DESC by update_time, mirroring
		// the SQL ORDER BY clause. The service preserves that order verbatim.
		listSessionsWithTitleRows: []mysqlstore.SessionWithTitle{
			{SessionID: "newest", UpdateTime: base.Add(2 * time.Hour)},
			{SessionID: "middle", UpdateTime: base.Add(1 * time.Hour)},
			{SessionID: "oldest", UpdateTime: base},
		},
	}
	svc := NewSessionService(newDeps(m, &fakeRedisStore{}, &fakeFSStore{}))

	got, err := svc.ListSessions(context.Background(), userID)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "newest", got[0].SessionID)
	assert.Equal(t, "middle", got[1].SessionID)
	assert.Equal(t, "oldest", got[2].SessionID)
}

func TestListSessions_EmptyUser(t *testing.T) {
	svc := NewSessionService(newDeps(&fakeMySQLStore{}, &fakeRedisStore{}, &fakeFSStore{}))
	got, err := svc.ListSessions(context.Background(), "nobody")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestEnsureSession_CreatesIfMissing(t *testing.T) {
	const (
		userID    = "u-7"
		sessionID = "s-7"
	)
	m := &fakeMySQLStore{} // GetSessionByID returns (nil, nil)
	r := &fakeRedisStore{}
	f := &fakeFSStore{}

	ctx := trace.WithContext(context.Background(), "tid-ensure")
	svc := NewSessionService(newDeps(m, r, f))

	err := svc.EnsureSession(ctx, userID, sessionID)
	require.NoError(t, err)

	require.Equal(t, 1, m.getSessionByIDCalls)
	require.Equal(t, 1, m.createSessionCalls)
	require.Equal(t, 1, f.ensureCalls)
	assert.Equal(t, sessionID, m.createSessionSession.SessionID)
	assert.Equal(t, userID, m.createSessionSession.UserID)
	assert.Equal(t, "tid-ensure", m.createSessionSession.TraceID, "trace_id from ctx must propagate")
}

func TestEnsureSession_AlreadyExists_NoOp(t *testing.T) {
	const (
		userID    = "u-8"
		sessionID = "s-8"
	)
	m := &fakeMySQLStore{getSessionByIDFound: &model.Session{SessionID: sessionID, UserID: userID}}
	r := &fakeRedisStore{}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	err := svc.EnsureSession(context.Background(), userID, sessionID)
	require.NoError(t, err)

	require.Equal(t, 1, m.getSessionByIDCalls)
	assert.Equal(t, 0, m.createSessionCalls, "create must NOT be called when session exists")
	require.Equal(t, 1, f.ensureCalls, "EnsureUserDirs still runs even when session exists")
}

func TestEnsureSession_GetLookupError_Returned(t *testing.T) {
	m := &fakeMySQLStore{getSessionIDErr: errors.New("boom")}
	svc := NewSessionService(newDeps(m, &fakeRedisStore{}, &fakeFSStore{}))
	err := svc.EnsureSession(context.Background(), "u", "s")
	require.Error(t, err)
	assert.Equal(t, 0, m.createSessionCalls)
}

func TestEnsureSession_CreateError_Returned(t *testing.T) {
	m := &fakeMySQLStore{createSessionErr: errors.New("dup")}
	svc := NewSessionService(newDeps(m, &fakeRedisStore{}, &fakeFSStore{}))
	err := svc.EnsureSession(context.Background(), "u", "s")
	require.Error(t, err)
}
