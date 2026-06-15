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
	m := &fakeMySQLStore{appendMessageID: 42, appendMessagesIDs: []int64{42}}
	r := &fakeRedisStore{}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	msg := sampleMessage(sessionID, "hello world")
	err := svc.SaveMessage(context.Background(), userID, msg)
	require.NoError(t, err)

	// Redis saw one batch append with the canonical JSON of the message.
	require.Equal(t, 1, r.appendMessagesCalls)
	require.Len(t, r.appendMessagesArgs, 1)
	var got model.Message
	require.NoError(t, json.Unmarshal(r.appendMessagesArgs[0], &got))
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

	// MySQL saw exactly one batch append with the original message struct.
	require.Equal(t, 1, m.appendMessagesCalls)
	require.Len(t, m.appendMessagesArg, 1)
	assert.Equal(t, msg, m.appendMessagesArg[0])
}

func TestSaveMessage_RedisFailure_DoesNotBlock(t *testing.T) {
	const (
		userID    = "u-2"
		sessionID = "s-2"
	)
	m := &fakeMySQLStore{appendMessagesIDs: []int64{1}}
	r := &fakeRedisStore{appendMessagesErr: errFake}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	err := svc.SaveMessage(context.Background(), userID, sampleMessage(sessionID, "x"))
	require.NoError(t, err, "redis failure must NOT surface to caller")

	require.Equal(t, 1, r.appendMessagesCalls)
	require.Equal(t, 1, f.writeCalls, "FS write must still happen")
	require.Equal(t, 1, m.appendMessagesCalls, "MySQL write must still happen")
}

func TestSaveMessage_MysqlFailure_LoggedNotReturned(t *testing.T) {
	const (
		userID    = "u-3"
		sessionID = "s-3"
	)
	m := &fakeMySQLStore{appendMessagesErr: errFake}
	r := &fakeRedisStore{}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	err := svc.SaveMessage(context.Background(), userID, sampleMessage(sessionID, "x"))
	require.NoError(t, err, "mysql failure must NOT surface to caller per design choice")

	require.Equal(t, 1, r.appendMessagesCalls, "redis still attempted")
	require.Equal(t, 1, f.writeCalls, "FS still attempted")
	require.Equal(t, 1, m.appendMessagesCalls, "MySQL attempt counted even on failure")
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

func TestSaveMessagesBatch_AllLayersSucceed(t *testing.T) {
	const (
		userID    = "u-batch-1"
		sessionID = "s-batch-1"
	)
	m := &fakeMySQLStore{appendMessagesIDs: []int64{10, 11, 12}}
	r := &fakeRedisStore{}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	msgs := []model.Message{
		sampleMessage(sessionID, "first"),
		sampleMessage(sessionID, "second"),
		sampleMessage(sessionID, "third"),
	}
	// Make the second and third rows assistant events so the batch exercises
	// mixed event types.
	msgs[1].Agent = model.AgentConfuse
	msgs[1].Role = model.RoleAssistant
	msgs[1].EventType = model.EventTypeToken
	msgs[1].MsgIndex = 1
	msgs[2].Agent = model.AgentConfuse
	msgs[2].Role = model.RoleAssistant
	msgs[2].EventType = model.EventTypeAgentEnd
	msgs[2].MsgIndex = 2

	err := svc.SaveMessagesBatch(context.Background(), userID, msgs)
	require.NoError(t, err)

	require.Equal(t, 1, r.appendMessagesCalls)
	require.Len(t, r.appendMessagesArgs, 3)

	require.Equal(t, 1, f.writeCalls)
	var doc sessionFile
	require.NoError(t, json.Unmarshal(f.writeData, &doc))
	require.Len(t, doc.Messages, 3)

	require.Equal(t, 1, m.appendMessagesCalls)
	require.Len(t, m.appendMessagesArg, 3)
	for i, msg := range msgs {
		assert.Equal(t, msg, m.appendMessagesArg[i])
	}
}

func TestSaveMessagesBatch_RedisFailure_DoesNotBlock(t *testing.T) {
	const (
		userID    = "u-batch-2"
		sessionID = "s-batch-2"
	)
	m := &fakeMySQLStore{appendMessagesIDs: []int64{1}}
	r := &fakeRedisStore{appendMessagesErr: errFake}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	err := svc.SaveMessagesBatch(context.Background(), userID, []model.Message{sampleMessage(sessionID, "x")})
	require.NoError(t, err, "redis batch failure must NOT surface to caller")

	require.Equal(t, 1, r.appendMessagesCalls)
	require.Equal(t, 1, f.writeCalls)
	require.Equal(t, 1, m.appendMessagesCalls)
}

func TestSaveMessagesBatch_MysqlFailure_LoggedNotReturned(t *testing.T) {
	const (
		userID    = "u-batch-3"
		sessionID = "s-batch-3"
	)
	m := &fakeMySQLStore{appendMessagesErr: errFake}
	r := &fakeRedisStore{}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	err := svc.SaveMessagesBatch(context.Background(), userID, []model.Message{sampleMessage(sessionID, "x")})
	require.NoError(t, err, "mysql batch failure must NOT surface to caller")

	require.Equal(t, 1, r.appendMessagesCalls)
	require.Equal(t, 1, f.writeCalls)
	require.Equal(t, 1, m.appendMessagesCalls)
}

func TestSaveMessagesBatch_FSFailure_Returned(t *testing.T) {
	const (
		userID    = "u-batch-4"
		sessionID = "s-batch-4"
	)
	m := &fakeMySQLStore{}
	r := &fakeRedisStore{}
	f := &fakeFSStore{readErr: errFake}
	svc := NewSessionService(newDeps(m, r, f))

	err := svc.SaveMessagesBatch(context.Background(), userID, []model.Message{sampleMessage(sessionID, "x")})
	require.Error(t, err)
}

func TestSaveMessagesBatch_Empty(t *testing.T) {
	svc := NewSessionService(newDeps(&fakeMySQLStore{}, &fakeRedisStore{}, &fakeFSStore{}))

	err := svc.SaveMessagesBatch(context.Background(), "u", []model.Message{})
	require.NoError(t, err)
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

func TestGetSessionMessages_PassesThrough(t *testing.T) {
	const sessionID = "s-10"
	want := []model.Message{
		{ID: 1, SessionID: sessionID, Content: "a"},
		{ID: 2, SessionID: sessionID, Content: "b"},
	}
	m := &fakeMySQLStore{listMessagesRows: want}
	svc := NewSessionService(newDeps(m, &fakeRedisStore{}, &fakeFSStore{}))

	msgs, next, err := svc.GetSessionMessages(context.Background(), sessionID, "", 10, "asc")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "a", msgs[0].Content)
	assert.Equal(t, "b", msgs[1].Content)
	assert.Empty(t, next)
}

func TestCreateSession_Success(t *testing.T) {
	const userID = "u-7"
	m := &fakeMySQLStore{}
	r := &fakeRedisStore{}
	f := &fakeFSStore{}

	ctx := trace.WithContext(context.Background(), "tid-create")
	svc := NewSessionService(newDeps(m, r, f))

	sessionID, err := svc.CreateSession(ctx, userID)
	require.NoError(t, err)

	require.Equal(t, 1, m.createSessionCalls)
	require.Equal(t, 1, f.ensureCalls)
	assert.Equal(t, userID, m.createSessionSession.UserID)
	assert.Equal(t, "tid-create", m.createSessionSession.TraceID, "trace_id from ctx must propagate")
	assert.Len(t, sessionID, 36, "session_id must be a 36-char UUID")
	assert.Equal(t, byte('7'), sessionID[14], "session_id must be UUID v7")
	assert.Equal(t, m.createSessionSession.SessionID, sessionID)
}

func TestCreateSession_EnsureUserDirsError_Returned(t *testing.T) {
	m := &fakeMySQLStore{}
	r := &fakeRedisStore{}
	f := &fakeFSStore{ensureErr: errors.New("no space")}
	svc := NewSessionService(newDeps(m, r, f))

	sessionID, err := svc.CreateSession(context.Background(), "u")
	require.Error(t, err)
	assert.Empty(t, sessionID)
	assert.Equal(t, 0, m.createSessionCalls, "mysql create must NOT be called when fs fails")
}

func TestCreateSession_CreateError_Returned(t *testing.T) {
	m := &fakeMySQLStore{createSessionErr: errors.New("dup")}
	svc := NewSessionService(newDeps(m, &fakeRedisStore{}, &fakeFSStore{}))
	sessionID, err := svc.CreateSession(context.Background(), "u")
	require.Error(t, err)
	assert.Empty(t, sessionID)
}

func TestGetSessionByID_PassesThrough(t *testing.T) {
	const sessionID = "s-9"
	want := &model.Session{SessionID: sessionID, UserID: "u-9"}
	m := &fakeMySQLStore{getSessionByIDFound: want}
	svc := NewSessionService(newDeps(m, &fakeRedisStore{}, &fakeFSStore{}))

	got, err := svc.GetSessionByID(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want.SessionID, got.SessionID)
	assert.Equal(t, want.UserID, got.UserID)
	require.Equal(t, 1, m.getSessionByIDCalls)
}
