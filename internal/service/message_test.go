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

// depsWithDrain builds SessionDeps with a drain hook attached.
func depsWithDrain(m *fakeMySQLStore, r *fakeRedisStore, f *fakeFSStore, d *fakeDrain) SessionDeps {
	deps := newDeps(m, r, f)
	deps.DrainMessageQueue = d.drain
	return deps
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
	d := &fakeDrain{}

	svc := NewMessageService(depsWithDrain(m, r, &fakeFSStore{}, d), nil)
	got, err := svc.RecoverMessages(context.Background(), userID, sessionID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, msgs[0], got[0])

	assert.Equal(t, 1, r.getCalls, "redis must be queried once")
	assert.Equal(t, 0, r.setCalls, "Redis backfill must NOT happen on redis hit")
	assert.Equal(t, 0, d.calls, "drain must NOT run on a redis hit")
	assert.Equal(t, 0, m.appendMessagesCalls)
}

// TestRecoverMessages_MissDrainsBeforeMySQLRead verifies the miss path runs
// the bounded drain BEFORE the MySQL read, then backfills the Redis cache.
func TestRecoverMessages_MissDrainsBeforeMySQLRead(t *testing.T) {
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
	d := &fakeDrain{}

	svc := NewMessageService(depsWithDrain(m, r, &fakeFSStore{}, d), nil)
	got, err := svc.RecoverMessages(context.Background(), userID, sessionID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, mysqlMsgs[0], got[0])
	assert.Equal(t, mysqlMsgs[1], got[1])

	assert.Equal(t, 1, r.getCalls)
	require.Equal(t, 1, d.calls, "miss must drain the write-behind queue first")
	require.Equal(t, 1, r.setCalls, "redis backfill from mysql")
	require.Len(t, r.setArgs, 2)
	assert.JSONEq(t, string(marshalled(t, mysqlMsgs[0])), string(r.setArgs[0]))
	assert.JSONEq(t, string(marshalled(t, mysqlMsgs[1])), string(r.setArgs[1]))
}

// TestRecoverMessages_DrainFailure_ProceedsToMySQL verifies a drain failure
// is logged (not surfaced) and the read still falls through to MySQL.
func TestRecoverMessages_DrainFailure_ProceedsToMySQL(t *testing.T) {
	const sessionID = "s-drain-err"
	m := &fakeMySQLStore{listMessagesRows: []model.Message{sampleMessage(sessionID, "row")}}
	d := &fakeDrain{err: errFake}

	svc := NewMessageService(depsWithDrain(m, &fakeRedisStore{}, &fakeFSStore{}, d), nil)
	got, err := svc.RecoverMessages(context.Background(), "u", sessionID)
	require.NoError(t, err, "drain failure must not fail the read")
	require.Len(t, got, 1)
	require.Equal(t, 1, d.calls)
}

// TestRecoverMessages_NilDrainHook_SkipsDrain verifies a nil drain hook
// (legacy wiring) still recovers from MySQL.
func TestRecoverMessages_NilDrainHook_SkipsDrain(t *testing.T) {
	const sessionID = "s-nil-drain"
	m := &fakeMySQLStore{listMessagesRows: []model.Message{sampleMessage(sessionID, "row")}}
	r := &fakeRedisStore{}

	svc := NewMessageService(newDeps(m, r, &fakeFSStore{}), nil)
	got, err := svc.RecoverMessages(context.Background(), "u", sessionID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 1, r.setCalls, "backfill still happens")
}

func TestRecoverMessages_AllEmpty_ReturnsEmpty(t *testing.T) {
	const sessionID = "s-4"
	m := &fakeMySQLStore{}
	r := &fakeRedisStore{}
	d := &fakeDrain{}

	svc := NewMessageService(depsWithDrain(m, r, &fakeFSStore{}, d), nil)
	got, err := svc.RecoverMessages(context.Background(), "u-4", sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Empty(t, got)

	assert.Equal(t, 1, r.getCalls)
	assert.Equal(t, 1, d.calls, "miss still drains (the queue may hold rows for this session)")
	assert.Equal(t, 0, r.setCalls, "no backfill when all tiers empty")
}

// TestRecoverMessages_RedisError_DrainsAndFallsThroughToMySQL verifies a
// Redis read error also takes the drain + MySQL path (Redis being down for
// the read does not skip the queue flush attempt).
func TestRecoverMessages_RedisError_DrainsAndFallsThroughToMySQL(t *testing.T) {
	const sessionID = "s-5"
	m := &fakeMySQLStore{listMessagesRows: []model.Message{sampleMessage(sessionID, "after-redis-err")}}
	r := &fakeRedisStore{getErr: errFake}
	d := &fakeDrain{}

	svc := NewMessageService(depsWithDrain(m, r, &fakeFSStore{}, d), nil)
	got, err := svc.RecoverMessages(context.Background(), "u-5", sessionID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "after-redis-err", got[0].Content)

	require.Equal(t, 1, d.calls, "redis error path must still attempt the drain")
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
