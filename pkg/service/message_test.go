package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/model"
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

	svc := NewMessageService(newDeps(m, r, &fakeFSStore{}), nil)
	got, err := svc.RecoverMessages(context.Background(), userID, sessionID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, msgs[0], got[0])

	assert.Equal(t, 1, r.getCalls, "redis must be queried once")
	assert.Equal(t, 0, r.setCalls, "Redis backfill must NOT happen on redis hit")
	assert.Equal(t, 0, m.appendMessagesCalls)
}

func TestRecoverMessages_RedisAndMySQL_FinalRowsAreIdentical(t *testing.T) {
	const sessionID = "s-final-rows"
	rows := []model.Message{
		{
			SessionID: sessionID, Agent: model.AgentUser, Role: model.RoleUser,
			EventType: model.EventTypeMessage, Content: "question", MsgIndex: 0,
		},
		{
			SessionID: sessionID, Agent: model.AgentConfucius, Role: model.RoleAssistant,
			EventType: model.EventTypeMessage, Content: "checking", ReasoningContent: "reason", MsgIndex: 1,
		},
		{
			SessionID: sessionID, Agent: model.AgentConfucius, Role: model.RoleAssistant,
			EventType: model.EventTypeToolCall, Content: "grep", ToolCallID: "c1", MsgIndex: 2,
		},
	}
	raws := make([][]byte, len(rows))
	for i := range rows {
		raws[i] = marshalled(t, rows[i])
	}

	redisSvc := NewMessageService(newDeps(
		&fakeMySQLStore{listMessagesRows: []model.Message{{Content: "must not be read"}}},
		&fakeRedisStore{getResult: raws},
		&fakeFSStore{},
	), nil)
	mysqlSvc := NewMessageService(newDeps(
		&fakeMySQLStore{listMessagesRows: rows},
		&fakeRedisStore{},
		&fakeFSStore{},
	), nil)

	fromRedis, err := redisSvc.RecoverMessages(context.Background(), "u", sessionID)
	require.NoError(t, err)
	fromMySQL, err := mysqlSvc.RecoverMessages(context.Background(), "u", sessionID)
	require.NoError(t, err)

	require.Equal(t, rows, fromRedis)
	require.Equal(t, fromRedis, fromMySQL)
	redisJSON, err := json.Marshal(fromRedis)
	require.NoError(t, err)
	mysqlJSON, err := json.Marshal(fromMySQL)
	require.NoError(t, err)
	assert.Equal(t, string(redisJSON), string(mysqlJSON))
}

// TestRecoverMessages_MissBackfillsRedisCache verifies the miss path reads
// MySQL and backfills the Redis cache.
func TestRecoverMessages_MissBackfillsRedisCache(t *testing.T) {
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

	svc := NewMessageService(newDeps(m, r, &fakeFSStore{}), nil)
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
}

func TestRecoverMessages_AllEmpty_ReturnsEmpty(t *testing.T) {
	const sessionID = "s-4"
	m := &fakeMySQLStore{}
	r := &fakeRedisStore{}

	svc := NewMessageService(newDeps(m, r, &fakeFSStore{}), nil)
	got, err := svc.RecoverMessages(context.Background(), "u-4", sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Empty(t, got)

	assert.Equal(t, 1, r.getCalls)
	assert.Equal(t, 0, r.setCalls, "no backfill when all tiers empty")
}

// TestRecoverMessages_RedisError_FallsThroughToMySQL verifies a Redis read
// error still serves the MySQL rows.
func TestRecoverMessages_RedisError_FallsThroughToMySQL(t *testing.T) {
	const sessionID = "s-5"
	m := &fakeMySQLStore{listMessagesRows: []model.Message{sampleMessage(sessionID, "after-redis-err")}}
	r := &fakeRedisStore{getErr: errFake}

	svc := NewMessageService(newDeps(m, r, &fakeFSStore{}), nil)
	got, err := svc.RecoverMessages(context.Background(), "u-5", sessionID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "after-redis-err", got[0].Content)
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
