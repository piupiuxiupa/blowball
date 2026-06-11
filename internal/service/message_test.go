package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

// marshalled returns the canonical JSON for m, failing the test on marshal error.
func marshalled(t *testing.T, m model.Message) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

func TestRecoverMessages_RedisHit_ShortCircuits(t *testing.T) {
	const (
		userID    = "u-1"
		sessionID = "s-1"
	)
	msgs := []model.Message{sampleMessage(sessionID, "hi")}
	raws := [][]byte{marshalled(t, msgs[0])}

	m := &fakeMySQLStore{listMessagesRows: []model.Message{sampleMessage(sessionID, "should-not-be-used")}}
	r := &fakeRedisStore{getResult: raws}
	f := &fakeFSStore{readResult: []byte("should-not-be-used")}

	svc := NewMessageService(newDeps(m, r, f), nil)
	got, err := svc.RecoverMessages(context.Background(), userID, sessionID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, msgs[0], got[0])

	assert.Equal(t, 1, r.getCalls, "redis must be queried once")
	assert.Equal(t, 0, f.writeCalls, "FS write must NOT happen on redis hit")
	assert.Equal(t, 0, r.setCalls, "Redis backfill must NOT happen on redis hit")
	assert.Equal(t, 0, m.appendMessageCalls)
}

func TestRecoverMessages_RedisMiss_FSHit_BackfillsRedis(t *testing.T) {
	const (
		userID    = "u-2"
		sessionID = "s-2"
	)
	msgs := []model.Message{sampleMessage(sessionID, "from-fs")}

	doc := sessionFile{SessionID: sessionID, Messages: []json.RawMessage{marshalled(t, msgs[0])}}
	fsBytes, err := json.Marshal(doc)
	require.NoError(t, err)

	m := &fakeMySQLStore{listMessagesRows: []model.Message{sampleMessage(sessionID, "should-not-be-used")}}
	r := &fakeRedisStore{}
	f := &fakeFSStore{readResult: fsBytes}

	svc := NewMessageService(newDeps(m, r, f), nil)
	got, err := svc.RecoverMessages(context.Background(), userID, sessionID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, msgs[0], got[0])

	assert.Equal(t, 1, r.getCalls)
	require.Equal(t, 1, r.setCalls, "redis must be backfilled from FS hit")
	require.Len(t, r.setArgs, 1)
	assert.JSONEq(t, string(marshalled(t, msgs[0])), string(r.setArgs[0]))

	assert.Equal(t, 0, f.writeCalls, "FS write should NOT happen on FS hit (no backfill needed)")
	assert.Equal(t, 0, m.appendMessageCalls)
}

func TestRecoverMessages_AllMiss_MySQLHit_BackfillsBoth(t *testing.T) {
	const (
		userID    = "u-3"
		sessionID = "s-3"
	)
	mysqlMsgs := []model.Message{
		sampleMessage(sessionID, "from-mysql-1"),
		sampleMessage(sessionID, "from-mysql-2"),
	}

	m := &fakeMySQLStore{listMessagesRows: mysqlMsgs}
	r := &fakeRedisStore{}
	f := &fakeFSStore{}

	svc := NewMessageService(newDeps(m, r, f), nil)
	got, err := svc.RecoverMessages(context.Background(), userID, sessionID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, mysqlMsgs[0], got[0])
	assert.Equal(t, mysqlMsgs[1], got[1])

	assert.Equal(t, 1, r.getCalls)
	require.Equal(t, 1, r.setCalls, "redis backfill from mysql")
	require.Len(t, r.setArgs, 2)
	assert.JSONEq(t, string(marshalled(t, mysqlMsgs[0])), string(r.setArgs[0]))
	assert.JSONEq(t, string(marshalled(t, mysqlMsgs[1])), string(r.setArgs[1]))

	require.Equal(t, 1, f.writeCalls, "FS backfill from mysql")
	var doc sessionFile
	require.NoError(t, json.Unmarshal(f.writeData, &doc))
	assert.Equal(t, sessionID, doc.SessionID)
	require.Len(t, doc.Messages, 2)
}

func TestRecoverMessages_AllEmpty_ReturnsEmpty(t *testing.T) {
	const (
		userID    = "u-4"
		sessionID = "s-4"
	)
	m := &fakeMySQLStore{}
	r := &fakeRedisStore{}
	f := &fakeFSStore{}

	svc := NewMessageService(newDeps(m, r, f), nil)
	got, err := svc.RecoverMessages(context.Background(), userID, sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Empty(t, got)

	assert.Equal(t, 1, r.getCalls)
	assert.Equal(t, 0, f.writeCalls, "no backfill when all tiers empty")
	assert.Equal(t, 0, r.setCalls, "no backfill when all tiers empty")
}

func TestRecoverMessages_RedisError_FallsThroughToFS(t *testing.T) {
	const (
		userID    = "u-5"
		sessionID = "s-5"
	)
	msgs := []model.Message{sampleMessage(sessionID, "fs-after-redis-err")}
	doc := sessionFile{SessionID: sessionID, Messages: []json.RawMessage{marshalled(t, msgs[0])}}
	fsBytes, err := json.Marshal(doc)
	require.NoError(t, err)

	m := &fakeMySQLStore{listMessagesRows: []model.Message{sampleMessage(sessionID, "should-not-be-used")}}
	r := &fakeRedisStore{getErr: errFake}
	f := &fakeFSStore{readResult: fsBytes}

	svc := NewMessageService(newDeps(m, r, f), nil)
	got, err := svc.RecoverMessages(context.Background(), userID, sessionID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, msgs[0], got[0])

	require.Equal(t, 1, r.setCalls, "redis backfill must happen after recovery via FS")
}

func TestMessageService_AppendMessage_Delegates(t *testing.T) {
	const userID = "u-6"
	deps := newDeps(&fakeMySQLStore{}, &fakeRedisStore{}, &fakeFSStore{})
	called := false
	save := func(_ context.Context, uid string, _ model.Message) error {
		called = true
		assert.Equal(t, userID, uid)
		return nil
	}
	svc := NewMessageService(deps, save)
	err := svc.AppendMessage(context.Background(), userID, sampleMessage("s", "x"))
	require.NoError(t, err)
	assert.True(t, called)
}
