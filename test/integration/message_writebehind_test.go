package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/msgflush"
)

// TestMessageWriteBehind_DualWriteMirrorsQueueAndCache verifies the write path
// shape before any flush happens: right after a turn, the read cache
// (msgs:{session_id}) and the ingest queue (msgs:buffer) hold the identical
// canonical blobs from ONE pipeline, the queue key carries no TTL, and MySQL
// has NOT been written synchronously.
func TestMessageWriteBehind_DualWriteMirrorsQueueAndCache(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"hi"},
			content:      "hi",
			finishReason: "stop",
			usage:        agent.Usage{TotalTokens: 1},
		},
		// The send-time title round routes to the scripted client's canned
		// title queue and never touches this queue.
	))
	ctx := context.Background()

	w := env.postMessage(`{"content":"wb-mirror"}`, authToken(t, defaultUserID))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Wait for the detached persistence goroutine's dual write to land in the
	// read cache.
	require.Eventually(t, func() bool {
		raws, err := env.redisSvc.GetMessages(ctx, defaultSessionID)
		return err == nil && len(raws) >= 2
	}, 2*time.Second, 10*time.Millisecond, "expected the turn to be dual-written")

	// Nothing reached MySQL synchronously — the flusher owns that write.
	assert.Empty(t, env.mysqlFake.messagesFor(defaultSessionID),
		"no synchronous MySQL write on the happy path")

	// The ingest queue holds the identical blobs, FIFO, with no TTL.
	queued, err := env.redisSvc.Client().LRange(ctx, "msgs:buffer", 0, -1).Result()
	require.NoError(t, err)
	assert.NotEmpty(t, queued, "ingest queue must hold the batch")

	cached, err := env.redisSvc.GetMessages(ctx, defaultSessionID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(queued), len(cached), "queue holds at least this turn's blobs")
	for i := range cached {
		assert.Equal(t, string(cached[i]), queued[len(queued)-len(cached)+i],
			"queue and cache elements must be the identical canonical blobs")
	}
	assert.Equal(t, time.Duration(0), env.miniRedis.TTL("msgs:buffer"),
		"the ingest queue must never carry a TTL")

	// Draining moves exactly the queued rows into MySQL once.
	before := env.mysqlFake.insertAttemptCount()
	require.NoError(t, env.drainMessages(ctx))
	rows := env.mysqlFake.messagesFor(defaultSessionID)
	assert.NotEmpty(t, rows)
	for _, m := range rows {
		assert.NotEmpty(t, m.ClientMsgID, "drained rows carry the idempotency key")
	}
	assert.Equal(t, before+len(rows), env.mysqlFake.insertAttemptCount())
}

// TestMessageWriteBehind_SendTimeBatchSplitsUserRow verifies the batch split
// on both Redis keys (send-time-user-message-persistence): while the turn is
// still running, msgs:buffer and msgs:{sid} each hold exactly the user row;
// after the turn ends, the event suffix appends to BOTH keys in order; and
// across the whole turn the user row appears exactly once per key.
func TestMessageWriteBehind_SendTimeBatchSplitsUserRow(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"slow ", "answer"},
			content:      "slow answer",
			finishReason: "stop",
			usage:        agent.Usage{TotalTokens: 2},
			tokenDelay:   100 * time.Millisecond,
		},
		scriptedLLMResponse{content: "T", finishReason: "stop", usage: agent.Usage{TotalTokens: 1}},
	)
	env := newTestEnv(t, llm)
	ctx := context.Background()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- env.postMessage(`{"content":"wb-split"}`, authToken(t, defaultUserID))
	}()

	// Mid-turn: both keys hold exactly the send-time user row.
	require.Eventually(t, func() bool {
		raws, err := env.redisSvc.GetMessages(ctx, defaultSessionID)
		return err == nil && len(raws) == 1
	}, 2*time.Second, 5*time.Millisecond, "the send-time user row must be the sole cache entry mid-turn")
	queuedMid, err := env.redisSvc.Client().LRange(ctx, "msgs:buffer", 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, queuedMid, 1, "mid-turn the ingest queue holds only the user row")
	assert.Contains(t, queuedMid[0], "wb-split")

	// Turn-end: the suffix appends to both keys; the user row appears once.
	<-done
	require.Eventually(t, func() bool {
		raws, err := env.redisSvc.GetMessages(ctx, defaultSessionID)
		return err == nil && len(raws) == 4
	}, 2*time.Second, 10*time.Millisecond, "user + 3 merged assistant events must land in the read cache")

	cached, err := env.redisSvc.GetMessages(ctx, defaultSessionID)
	require.NoError(t, err)
	queued, err := env.redisSvc.Client().LRange(ctx, "msgs:buffer", 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, queued, 4, "queue and cache receive the same blobs from both batches")

	userRows := 0
	for i, raw := range cached {
		var m model.Message
		require.NoError(t, json.Unmarshal(raw, &m))
		if m.EventType == model.EventTypeMessage {
			userRows++
			assert.Equal(t, 0, m.MsgIndex, "the user row keeps msg_index 0")
		}
		// Queue and cache elements are the identical canonical blobs, in
		// order, across the two batches.
		assert.Equal(t, string(raw), queued[i], "queue/cache blob %d must match", i)
	}
	assert.Equal(t, 1, userRows, "the user row must appear exactly once in msgs:{sid}")

	// Order: user first, then the merged assistant events.
	var first model.Message
	require.NoError(t, json.Unmarshal(cached[0], &first))
	assert.Equal(t, model.RoleUser, first.Role)
	var last model.Message
	require.NoError(t, json.Unmarshal(cached[len(cached)-1], &last))
	assert.Equal(t, model.EventTypeAgentEnd, last.EventType)
}

// TestMessageWriteBehind_BackgroundFlusherLandsTurn runs a REAL
// msgflush.Flusher (the agent/all-role wiring shape: miniredis-backed
// redis.Store + memoryMySQL) against the queue and verifies a posted turn
// reaches the MySQL tier within one flush interval without any explicit
// drain, then that the queue is empty.
func TestMessageWriteBehind_BackgroundFlusherLandsTurn(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"flushed"},
			content:      "flushed",
			finishReason: "stop",
			usage:        agent.Usage{TotalTokens: 1},
		},
		scriptedLLMResponse{content: "T", finishReason: "stop", usage: agent.Usage{TotalTokens: 1}},
	))
	ctx := context.Background()

	notify := make(chan struct{}, 1)
	flusher := msgflush.NewFlusher(env.redisSvc, env.mysqlFake, msgflush.Config{
		Interval:  50 * time.Millisecond,
		BatchSize: 10,
	}, notify)
	flusher.Start()
	defer flusher.Close()

	w := env.postMessage(`{"content":"wb-flush"}`, authToken(t, defaultUserID))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Eventually(t, func() bool {
		return len(env.mysqlFake.messagesFor(defaultSessionID)) >= 2
	}, 2*time.Second, 10*time.Millisecond, "expected the flusher to land the turn in MySQL")

	require.Eventually(t, func() bool {
		n, err := env.redisSvc.LenMessageBuffer(ctx)
		return err == nil && n == 0
	}, 2*time.Second, 10*time.Millisecond, "queue must drain")

	rows := env.mysqlFake.messagesFor(defaultSessionID)
	assert.Equal(t, "wb-flush", rows[0].Content)
	assert.Equal(t, model.RoleUser, rows[0].Role)

	// The flusher refreshed update_time for the session (the synchronous
	// per-turn refresh moved into the flush path).
	env.mysqlFake.mu.Lock()
	_, refreshed := env.mysqlFake.sessions[defaultSessionID]
	env.mysqlFake.mu.Unlock()
	assert.True(t, refreshed)
}

// TestMessageWriteBehind_RedisFailureFallsBackToDirectWrite makes Redis
// command execution fail wholesale (miniredis global error injection — the
// "Redis is down" shape) and verifies the batch degrades to the synchronous
// direct MySQL write: rows land immediately and carry the idempotency key,
// while the response stream is unaffected.
func TestMessageWriteBehind_RedisFailureFallsBackToDirectWrite(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"fallback"},
			content:      "fallback",
			finishReason: "stop",
			usage:        agent.Usage{TotalTokens: 1},
		},
		scriptedLLMResponse{content: "T", finishReason: "stop", usage: agent.Usage{TotalTokens: 1}},
	))

	// Fail every Redis command from here on — the dual write pipeline cannot
	// commit, and every read-side Redis op degrades to its fallback too.
	env.miniRedis.SetError("simulated redis outage")

	w := env.postMessage(`{"content":"wb-fallback"}`, authToken(t, defaultUserID))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Eventually(t, func() bool {
		return len(env.mysqlFake.messagesFor(defaultSessionID)) >= 2
	}, 2*time.Second, 10*time.Millisecond, "expected the fallback direct write to land the batch")

	rows := env.mysqlFake.messagesFor(defaultSessionID)
	assert.Equal(t, "wb-fallback", rows[0].Content)
	for _, m := range rows {
		assert.NotEmpty(t, m.ClientMsgID, "fallback rows carry the idempotency key")
	}
}

// TestMessageWriteBehind_RecoverMissDrainsThenBackfills verifies the read-miss
// path: with the cache empty but the queue still holding a turn, a
// RecoverMessages call drains first, reads the complete history from MySQL,
// and backfills the cache — the queued rows are not erased by the backfill.
func TestMessageWriteBehind_RecoverMissDrainsThenBackfills(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())
	ctx := context.Background()

	// Seed a turn straight into the queue (dual-written but unflushed) and
	// leave the read cache empty — the 24h-idle / post-restart shape.
	msgs := []model.Message{
		{SessionID: defaultSessionID, Agent: model.AgentUser, MsgIndex: 0, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "queued-only", TraceID: "t", ClientMsgID: "wb-recover-1"},
	}
	raws := make([][]byte, 0, len(msgs))
	for _, m := range msgs {
		raws = append(raws, mustMarshal(t, m))
	}
	require.NoError(t, env.redisSvc.PushMessageBuffer(ctx, raws))

	recovered, err := env.msgSvc.RecoverMessages(ctx, defaultUserID, defaultSessionID)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	assert.Equal(t, "queued-only", recovered[0].Content,
		"the miss path must drain the queue before the MySQL read")

	// The cache was backfilled from the (now complete) MySQL rows.
	cached, err := env.redisSvc.GetMessages(ctx, defaultSessionID)
	require.NoError(t, err)
	require.Len(t, cached, 1)
	assert.Contains(t, string(cached[0]), "queued-only")

	// And the queue is empty afterwards.
	n, err := env.redisSvc.LenMessageBuffer(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}

// TestMessageWriteBehind_DoubleDrainInsertsOnce verifies end-to-end
// idempotency: draining the same logical batch twice (the crash-recovery
// replay shape) inserts every row once thanks to the UNIQUE client_msg_id +
// INSERT IGNORE semantics.
func TestMessageWriteBehind_DoubleDrainInsertsOnce(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())
	ctx := context.Background()

	msgs := []model.Message{
		{SessionID: defaultSessionID, Agent: model.AgentUser, MsgIndex: 0, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "idem", TraceID: "t", ClientMsgID: "wb-idem-1"},
		{SessionID: defaultSessionID, Agent: "Confucius", MsgIndex: 1, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "potent", TraceID: "t", ClientMsgID: "wb-idem-2"},
	}
	raws := make([][]byte, 0, len(msgs))
	for _, m := range msgs {
		raws = append(raws, mustMarshal(t, m))
	}
	// Deliver the same batch twice, simulating a crash between the committed
	// INSERT and the LREM ack followed by startup recovery.
	require.NoError(t, env.redisSvc.PushMessageBuffer(ctx, raws))
	require.NoError(t, env.redisSvc.PushMessageBuffer(ctx, raws))

	require.NoError(t, env.drainMessages(ctx))

	rows := env.mysqlFake.messagesFor(defaultSessionID)
	require.Len(t, rows, 2, "duplicate delivery must collapse to one row per client_msg_id")
	assert.Equal(t, 4, env.mysqlFake.insertAttemptCount(),
		"both deliveries were attempted; both duplicates ignored")
}
