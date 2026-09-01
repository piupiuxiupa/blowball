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

// TestSaveMessage_DualWrite_NoSyncMySQL verifies the Redis-first happy path:
// one dual-write pipeline call carries the canonical JSON (with a freshly
// minted client_msg_id) to both the read cache and the ingest queue, and NO
// synchronous MySQL write or FS write happens.
func TestSaveMessage_DualWrite_NoSyncMySQL(t *testing.T) {
	const (
		userID    = "u-1"
		sessionID = "s-1"
	)
	m := &fakeMySQLStore{}
	r := &fakeRedisStore{}
	f := &fakeFSStore{}
	svc := NewSessionService(newDeps(m, r, f))

	msg := sampleMessage(sessionID, "hello world")
	err := svc.SaveMessage(context.Background(), userID, msg)
	require.NoError(t, err)

	// One dual write carrying exactly one element.
	require.Equal(t, 1, r.dualCalls)
	assert.Equal(t, sessionID, r.dualSID)
	require.Len(t, r.dualArgs, 1)

	// The queued blob carries a minted idempotency key and otherwise
	// round-trips the message (SaveMessage copies the struct into its batch
	// slice, so the mint lands on the persisted row, not the caller's value).
	var got model.Message
	require.NoError(t, json.Unmarshal(r.dualArgs[0], &got))
	require.NotEmpty(t, got.ClientMsgID, "client_msg_id must be minted at persistence time")
	assert.Equal(t, msg.Content, got.Content)
	assert.Equal(t, msg.SessionID, got.SessionID)

	// The flusher owns the MySQL write now: no synchronous insert, no
	// synchronous update_time, no FS touch.
	assert.Equal(t, 0, m.appendMessagesCalls, "no synchronous MySQL write on the happy path")
	assert.Equal(t, 0, m.updateSessionTimeCalls, "update_time refresh belongs to the flusher")
	assert.Equal(t, 0, f.ensureCalls, "FS store is not part of the message write path")
}

// TestSaveMessage_PreStampedClientMsgIDPreserved verifies an
// already-stamped message keeps its key (redelivery stays idempotent).
func TestSaveMessage_PreStampedClientMsgIDPreserved(t *testing.T) {
	const sessionID = "s-pre"
	r := &fakeRedisStore{}
	svc := NewSessionService(newDeps(&fakeMySQLStore{}, r, &fakeFSStore{}))

	msg := sampleMessage(sessionID, "x")
	msg.ClientMsgID = "cmid-fixed"
	require.NoError(t, svc.SaveMessage(context.Background(), "u", msg))
	assert.Equal(t, "cmid-fixed", msg.ClientMsgID)

	var got model.Message
	require.NoError(t, json.Unmarshal(r.dualArgs[0], &got))
	assert.Equal(t, "cmid-fixed", got.ClientMsgID)
}

// TestSaveMessage_RedisFailure_FallsBackToDirectMySQL verifies the fallback:
// when the dual-write pipeline fails, the batch is synchronously
// inserted into MySQL (idempotent) plus one update_time refresh, and the
// caller still sees success.
func TestSaveMessage_RedisFailure_FallsBackToDirectMySQL(t *testing.T) {
	const (
		userID    = "u-2"
		sessionID = "s-2"
	)
	m := &fakeMySQLStore{}
	r := &fakeRedisStore{dualErr: errFake}
	svc := NewSessionService(newDeps(m, r, &fakeFSStore{}))

	err := svc.SaveMessage(context.Background(), userID, sampleMessage(sessionID, "x"))
	require.NoError(t, err, "redis failure must degrade, not surface")

	require.Equal(t, 1, r.dualCalls)
	require.Equal(t, 1, m.appendMessagesCalls, "fallback must write MySQL synchronously")
	require.Len(t, m.appendMessagesArg, 1)
	require.NotEmpty(t, m.appendMessagesArg[0].ClientMsgID, "fallback row carries the idempotency key")
	require.Equal(t, 1, m.updateSessionTimeCalls, "fallback refreshes update_time (the flusher never sees this batch)")
	assert.Equal(t, sessionID, m.updateSessionTimeArg)
}

// TestSaveMessage_BothTiersFail_ErrorReturned verifies that when Redis AND
// MySQL are both down the failure is RETURNED (not swallowed) so callers can
// run their at-least-once recovery — the send-time path defers the user row
// to the turn-end batch, the turn-end path retries within its bounded budget.
func TestSaveMessage_BothTiersFail_ErrorReturned(t *testing.T) {
	const sessionID = "s-3"
	m := &fakeMySQLStore{appendMessagesErr: errFake}
	r := &fakeRedisStore{dualErr: errFake}
	svc := NewSessionService(newDeps(m, r, &fakeFSStore{}))

	err := svc.SaveMessage(context.Background(), "u-3", sampleMessage(sessionID, "x"))
	require.ErrorIs(t, err, errFake, "dual-tier failure must surface to the caller")
	require.Equal(t, 1, m.appendMessagesCalls, "fallback attempt still made")
}

// TestSaveMessagesBatch_MixedEvents_DualWrite verifies a multi-message batch
// (user message + assistant events) lands as ONE dual write with all rows in
// order and each carrying a distinct client_msg_id.
func TestSaveMessagesBatch_MixedEvents_DualWrite(t *testing.T) {
	const (
		userID    = "u-batch-1"
		sessionID = "s-batch-1"
	)
	m := &fakeMySQLStore{}
	r := &fakeRedisStore{}
	svc := NewSessionService(newDeps(m, r, &fakeFSStore{}))

	msgs := []model.Message{
		sampleMessage(sessionID, "first"),
		sampleMessage(sessionID, "second"),
		sampleMessage(sessionID, "third"),
	}
	msgs[1].Agent = model.AgentConfucius
	msgs[1].Role = model.RoleAssistant
	msgs[1].EventType = model.EventTypeToken
	msgs[1].MsgIndex = 1
	msgs[2].Agent = model.AgentConfucius
	msgs[2].Role = model.RoleAssistant
	msgs[2].EventType = model.EventTypeAgentEnd
	msgs[2].MsgIndex = 2

	err := svc.SaveMessagesBatch(context.Background(), userID, msgs)
	require.NoError(t, err)

	require.Equal(t, 1, r.dualCalls)
	require.Len(t, r.dualArgs, 3)

	ids := map[string]struct{}{}
	for i, raw := range r.dualArgs {
		var got model.Message
		require.NoError(t, json.Unmarshal(raw, &got))
		assert.Equal(t, msgs[i], got, "queued row %d must round-trip", i)
		require.NotEmpty(t, got.ClientMsgID)
		ids[got.ClientMsgID] = struct{}{}
	}
	assert.Len(t, ids, 3, "every row gets a distinct client_msg_id")

	assert.Equal(t, 0, m.appendMessagesCalls)
	assert.Equal(t, 0, m.updateSessionTimeCalls)
}

// TestSaveMessagesBatch_Empty verifies an empty batch is a full no-op.
func TestSaveMessagesBatch_Empty(t *testing.T) {
	r := &fakeRedisStore{}
	svc := NewSessionService(newDeps(&fakeMySQLStore{}, r, &fakeFSStore{}))

	require.NoError(t, svc.SaveMessagesBatch(context.Background(), "u", []model.Message{}))
	assert.Equal(t, 0, r.dualCalls)
}

func TestListSessions_WithTitle(t *testing.T) {
	const userID = "u-5"
	m := &fakeMySQLStore{
		listSessionsWithTitleRows: []mysqlstore.SessionWithTitle{
			{SessionID: "s-a", UserID: userID, Title: "Alpha", UpdateTime: time.Unix(2, 0).UTC()},
			{SessionID: "s-b", UserID: userID, Title: "", UpdateTime: time.Unix(1, 0).UTC()},
		},
	}
	svc := NewSessionService(newDeps(m, &fakeRedisStore{}, &fakeFSStore{}))

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
	f := &fakeFSStore{ensureErr: errors.New("no space")}
	svc := NewSessionService(newDeps(m, &fakeRedisStore{}, f))

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
