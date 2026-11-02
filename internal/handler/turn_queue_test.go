package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/stream"
)

// queueTestEntry builds a queued entry + its meta + the queued event, as the
// enqueue path would leave them (session-message-queue): a running turn
// (trace-1) holds the claim, and the queued message waits behind it.
func seedQueuedRun(t *testing.T, env *turnRunTestEnv, runID string, newlyClaimed ...string) run.QueueEntry {
	t.Helper()
	ctx := context.Background()
	entry := run.QueueEntry{
		RunID:        runID,
		SessionID:    "sess-1",
		UserID:       "user-1",
		Content:      "queued " + runID,
		Model:        "gpt-test",
		NewlyClaimed: newlyClaimed,
		EnqueuedAt:   time.Unix(1700000000, 0).UTC(),
	}
	// A running turn holds the session claim so admission queues.
	_, _, err := env.store.ClaimSession(ctx, "sess-1", "trace-1")
	require.NoError(t, err)
	res, err := env.store.AdmitMessage(ctx, "sess-1", entry, 8)
	require.NoError(t, err)
	require.Equal(t, run.AdmitQueued, res.Outcome)
	require.NoError(t, env.store.InitMeta(ctx, runID, run.RunMeta{
		SessionID: "sess-1", UserID: "user-1",
		Status: run.StatusQueued, Model: "gpt-test",
		CreatedAt: entry.EnqueuedAt.Format(time.RFC3339),
	}))
	require.NoError(t, env.store.AppendEvent(ctx, runID, stream.QueuedEvent(runID, res.Position)))
	return entry
}

// TestCancelTurn_QueuedRun covers the queue-cancel branch: the entry is
// dequeued, the run finalizes as cancelled (NOT interrupted — the dead-run
// ladder must never see a queued run), the running turn's claim is untouched,
// the staged-attachment rollback hook fires with the entry, and a second
// cancel is idempotent.
func TestCancelTurn_QueuedRun(t *testing.T) {
	env := newTurnRunEnv(t, newBlockingStub())
	ctx := context.Background()
	seedQueuedRun(t, env, "trace-9", "att-1", "att-2")

	var rolledBack []run.QueueEntry
	env.turnH.SetQueueCancelRollback(func(_ context.Context, entry run.QueueEntry) {
		rolledBack = append(rolledBack, entry)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/turns/trace-9/cancel", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), `"status":"cancelled"`, "body: %s", w.Body.String())

	// Run state: cancelled + retired; NOT interrupted.
	meta, ok, err := env.store.GetMeta(ctx, "trace-9")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, run.StatusCancelled, meta.Status)
	// The queue is empty and the running turn still holds its claim.
	n, err := env.store.QueueLen(ctx, "sess-1")
	require.NoError(t, err)
	require.Zero(t, n, "queue not drained by the cancel")
	runs, _ := env.store.ActiveRuns(ctx, []string{"sess-1"})
	require.Equal(t, "trace-1", runs["sess-1"], "cancel of a queued run must not touch the running turn's claim")

	// The rollback hook fired exactly once with the entry's newly-claimed ids.
	require.Len(t, rolledBack, 1)
	require.Equal(t, []string{"att-1", "att-2"}, rolledBack[0].NewlyClaimed)

	// Second cancel: terminal → idempotent 200 with the terminal status.
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/turns/trace-9/cancel", nil)
	w2 := httptest.NewRecorder()
	env.engine.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)
	require.Contains(t, w2.Body.String(), `"status":"cancelled"`)
	require.Len(t, rolledBack, 1, "idempotent cancel must not re-run the rollback")
}

// TestCancelTurn_QueuedRunDequeueRace covers the dequeue-miss race: the entry
// is gone (a pump popped it and holds the claim) while the stale meta still
// reads queued — the endpoint answers from the CURRENT status (queued) and
// must NOT relabel the starting run via the dead-run force-clear.
func TestCancelTurn_QueuedRunDequeueRace(t *testing.T) {
	env := newTurnRunEnv(t, newBlockingStub())
	ctx := context.Background()
	seedQueuedRun(t, env, "trace-9")

	// The pump pops the entry and transfers the claim; SetStatus(running)
	// happens later in startTurn, so the meta still reads queued here —
	// exactly the window a cancel can land in.
	require.NoError(t, env.store.ReleaseSession(ctx, "sess-1", "trace-1"))
	pumped, ok, err := env.store.PumpHead(ctx, "sess-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "trace-9", pumped.RunID)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/turns/trace-9/cancel", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	// The honest answer is the current (still-queued) status — not a
	// fabricated cancelled, and not an interrupted from the dead-run ladder.
	require.Contains(t, w.Body.String(), `"status":"queued"`, "body: %s", w.Body.String())
	meta, _, _ := env.store.GetMeta(ctx, "trace-9")
	require.Equal(t, run.StatusQueued, meta.Status, "dequeue-miss must not relabel a starting run")
	// The pump's claim transfer survives; the client retries the cancel once
	// the status flips to running and takes the ordinary registry path.
	runs, _ := env.store.ActiveRuns(ctx, []string{"sess-1"})
	require.Equal(t, "trace-9", runs["sess-1"])
}

// TestTurnEvents_QueuedRunNotMisclassifiedAsDead pins writeRunEventStream's
// dead-run guard: a queued run has NO heartbeat by design, and the subscriber
// must keep tailing (replaying the queued event) instead of synthesizing an
// interrupted done. Flipping the status to running lets the same connection
// continue into the turn's events without replay duplication.
func TestTurnEvents_QueuedRunNotMisclassifiedAsDead(t *testing.T) {
	env := newTurnRunEnv(t, newBlockingStub())
	ctx := context.Background()
	seedQueuedRun(t, env, "trace-9")

	ctxReq, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-1/turns/trace-9/events", nil).WithContext(ctxReq)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		env.engine.ServeHTTP(w, req)
		close(done)
	}()

	// Give the subscriber time to replay and poll; the only replayable event
	// is the queued one, and no interrupted synthesis may appear.
	time.Sleep(300 * time.Millisecond)
	body := w.Body.String()
	require.Contains(t, body, "event: queued", "queued event not replayed: %s", body)
	require.NotContains(t, body, "interrupted", "queued run misclassified as dead: %s", body)
	select {
	case <-done:
		t.Fatal("subscriber closed a queued run's stream (should keep tailing)")
	default:
	}

	// The pump starts the turn: status flips to running and events flow. The
	// SAME connection must continue without a duplicate replay of queued.
	require.NoError(t, env.store.SetStatus(ctx, "trace-9", run.StatusRunning))
	require.NoError(t, env.store.AppendEvent(ctx, "trace-9", stream.AgentStartEvent(stream.AgentConfucius)))
	require.NoError(t, env.store.AppendEvent(ctx, "trace-9", stream.DoneEvent(nil)))
	require.NoError(t, env.store.SetStatus(ctx, "trace-9", run.StatusDone))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber never closed after the run went terminal")
	}
	finalBody := w.Body.String()
	require.Equal(t, 1, strings.Count(finalBody, "event: queued"), "queued event replayed twice: %s", finalBody)
	require.Equal(t, 1, strings.Count(finalBody, "event: agent_start"))
	require.Equal(t, 1, strings.Count(finalBody, "event: done"))
}

// ── full queue admission / continuation tests (session-message-queue) ──────

// queueStubOrchestrator is a multi-turn stub: unlike blockingStubOrchestrator
// (whose done channel makes it single-shot), every Handle call emits the
// canned event sequence around a shared release gate, and an invocation
// counter lets tests observe exactly which messages have started their turns.
type queueStubOrchestrator struct {
	release chan struct{}
	started atomic.Int64
}

func newQueueStub() *queueStubOrchestrator {
	return &queueStubOrchestrator{release: make(chan struct{})}
}

func (s *queueStubOrchestrator) Handle(ctx context.Context, _, _, _ string, _ []agent.Message, hub *stream.Hub, _ TurnHooks, _ agent.ModelOverride) ([]stream.StreamEvent, map[string]any, error) {
	s.started.Add(1)
	hub.Send(stream.AgentStartEvent(stream.AgentConfucius))
	hub.Send(stream.TokenEvent(stream.AgentConfucius, "partial"))
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	hub.Send(stream.TokenEvent(stream.AgentConfucius, " done"))
	hub.Send(stream.DoneEvent(nil))
	// The RETURNED events are what the turn-end persistence batch persists
	// (the adapter's collected stream), so give them real content: the
	// terminal batch then carries one assistant row per turn.
	return []stream.StreamEvent{
		stream.AgentStartEvent(stream.AgentConfucius),
		stream.TokenEvent(stream.AgentConfucius, "partial done"),
	}, nil, nil
}

// queueEnv mirrors turnRunTestEnv with the queue ENABLED and a per-request
// trace counter (trace-1, trace-2, ...) so multi-message tests can address
// each run. msgSvc is exposed for the wait-period invisibility assertion.
type queueEnv struct {
	engine  *gin.Engine
	store   run.Store
	reg     *run.Registry
	stub    *queueStubOrchestrator
	streamH *MessageStreamHandler
	msgSvc  *service.MessageService
	mysql   *handlerFakeMySQL
	redis   *handlerFakeRedis
	next    func() string
}

// newQueueEnvWithStore builds the env over an arbitrary run.Store so tests
// can wrap the MemStore (e.g. admission transport failures).
func newQueueEnvWithStore(t *testing.T, stub *queueStubOrchestrator, maxDepth int, store run.Store) *queueEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mysql := &handlerFakeMySQL{getSessionByIDFound: &model.Session{SessionID: "sess-1", UserID: "user-1"}}
	redis := &handlerFakeRedis{}
	deps := sessionDeps(mysql, redis, newHandlerFakeFS())
	sessSvc := newSessionSvc(deps)
	msgSvc := newMessageSvc(deps)
	titleSvc := newTitleSvcWithFake(t, deps, "T")

	reg := run.NewRegistry()
	runs := run.NewManager(store, reg)
	streamH := NewMessageStreamHandler(sessSvc, msgSvc, titleSvc, nil, nil, stub, "/tmp/blowball-test-data", runs, testSelectionConfig(), 0)
	streamH.EnableMessageQueue(maxDepth)
	turnH := NewTurnRunHandler(runs)
	turnH.SetQueueCancelRollback(streamH.RollbackQueuedAttachments)

	var n atomic.Int64
	next := func() string { return "trace-" + strconv.FormatInt(n.Add(1), 10) }

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, next())
		c.Next()
	})
	r.POST("/api/v1/sessions/:session_id/messages", streamH.SendMessage)
	r.POST("/api/v1/sessions/:session_id/turns/:run_id/cancel", turnH.CancelTurn)
	r.GET("/api/v1/sessions/:session_id/turns/:run_id/events", turnH.TurnEvents)
	return &queueEnv{engine: r, store: store, reg: reg, stub: stub, streamH: streamH, msgSvc: msgSvc, mysql: mysql, redis: redis, next: next}
}

func newQueueEnv(t *testing.T, stub *queueStubOrchestrator, maxDepth int) *queueEnv {
	return newQueueEnvWithStore(t, stub, maxDepth, run.NewMemStore())
}

func (e *queueEnv) postMessage(sessionID, content string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/"+sessionID+"/messages",
		strings.NewReader(`{"content":"`+content+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	return w
}

func (e *queueEnv) postMessageAsync(sessionID, content string) chan *httptest.ResponseRecorder {
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() { out <- e.postMessage(sessionID, content) }()
	return out
}

func (e *queueEnv) awaitQueueDepth(t *testing.T, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		n, _ := e.store.QueueLen(context.Background(), "sess-1")
		return n == want
	}, 2*time.Second, 5*time.Millisecond, "queue depth never reached %d", want)
}

func (e *queueEnv) awaitClaim(t *testing.T, wantNonEmpty bool) string {
	t.Helper()
	require.Eventually(t, func() bool {
		runs, _ := e.store.ActiveRuns(context.Background(), []string{"sess-1"})
		return (runs["sess-1"] != "") == wantNonEmpty
	}, 2*time.Second, 5*time.Millisecond)
	runs, _ := e.store.ActiveRuns(context.Background(), []string{"sess-1"})
	return runs["sess-1"]
}

// TestQueue_AdmissionAndSnapshot: while a turn runs, a second POST is
// accepted into the queue — the entry snapshot carries the resolved request
// and the waiting message is NOT yet visible to history reads (design D4:
// the user row lands at pump time).
func TestQueue_AdmissionAndSnapshot(t *testing.T) {
	env := newQueueEnv(t, newQueueStub(), 8)

	// Turn A holds the slot.
	wA := env.postMessageAsync("sess-1", "message A")
	env.awaitClaim(t, true)

	// Turn B is admitted into the queue; its SSE stays open (tailing).
	wB := env.postMessageAsync("sess-1", "message B")
	env.awaitQueueDepth(t, 1)

	// Entry snapshot: resolved request fields, arrival time recorded.
	entries, err := env.store.QueueEntries(context.Background(), "sess-1")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "trace-2", entries[0].RunID)
	require.Equal(t, "message B", entries[0].Content)
	require.Equal(t, "user-1", entries[0].UserID)
	require.False(t, entries[0].EnqueuedAt.IsZero())
	require.Equal(t, "test-model", entries[0].Model) // testSelectionConfig's entry
	require.NotZero(t, entries[0].Selection.MaxContextTokens)

	// The queued run's meta is queued and its log holds the queued event.
	meta, ok, _ := env.store.GetMeta(context.Background(), "trace-2")
	require.True(t, ok)
	require.Equal(t, run.StatusQueued, meta.Status)
	evs, _ := env.store.ReadEvents(context.Background(), "trace-2", "0", 0)
	require.NotEmpty(t, evs)
	require.Equal(t, stream.EventQueued, evs[0].Event.Type)
	require.Equal(t, "trace-2", evs[0].Event.Meta[stream.MetaRunID])

	// Wait-period invisibility (design D4): nothing the history read paths
	// consume holds the queued message yet — the Redis dual-write cache (the
	// send-time user row's landing spot) has no "message B" row. It lands at
	// pump time, asserted in TestQueue_Continuation.
	for _, m := range env.redis.dualSnapshot() {
		require.NotContains(t, m.Content, "message B", "queued message visible to history reads before its turn started")
	}
	// The synchronous MySQL write is the other durable surface the mid-turn
	// compaction recovery (RecoverDurable) reads; an enqueue-time user-row
	// write would land here and leak the queued message into the RUNNING
	// turn's rewritten round (design D4 hazard #1). The fake records the
	// last direct-write batch — assert it is A's user row, not B's.
	require.NotNil(t, env.mysql.appendMessagesArg)
	for _, m := range env.mysql.appendMessagesArg {
		require.NotContains(t, m.Content, "message B",
			"queued message hit the durable store before its turn started (mid-turn compaction leak)")
	}

	// Cleanup: release turn A, let B run and finish.
	close(env.stub.release)
	select {
	case <-wA:
	case <-time.After(5 * time.Second):
		t.Fatal("turn A never completed")
	}
	select {
	case <-wB:
	case <-time.After(5 * time.Second):
		t.Fatal("queued turn B never completed")
	}
}

// TestQueue_DepthCap: beyond max_depth the POST is rejected 409 QUEUE_FULL
// and the queue is unchanged.
func TestQueue_DepthCap(t *testing.T) {
	env := newQueueEnv(t, newQueueStub(), 1)

	env.postMessageAsync("sess-1", "message A")
	env.awaitClaim(t, true)
	env.postMessageAsync("sess-1", "message B")
	env.awaitQueueDepth(t, 1)

	// The queue is full (cap 1): message C is rejected.
	wC := env.postMessage("sess-1", "message C")
	require.Equal(t, http.StatusConflict, wC.Code, "body: %s", wC.Body.String())
	require.Contains(t, wC.Body.String(), "QUEUE_FULL")
	n, _ := env.store.QueueLen(context.Background(), "sess-1")
	require.Equal(t, 1, n, "QUEUE_FULL must not change the queue")

	close(env.stub.release)
}

// TestQueue_AdmissionFailureDegrades: a transport failure on AdmitMessage
// falls back to the standing claim-failure convention — the message runs
// immediately, unguarded, and no queue entry is left behind.
func TestQueue_AdmissionFailureDegrades(t *testing.T) {
	inner := run.NewMemStore()
	wrapped := &failingAdmitStore{MemStore: inner}
	env := newQueueEnvWithStore(t, newQueueStub(), 8, wrapped)

	// Turn A: admission fails → WARN → unguarded immediate start (no claim
	// is ever taken — the claim-failure convention, ported).
	wA := env.postMessageAsync("sess-1", "message A")
	require.Eventually(t, func() bool {
		return env.stub.started.Load() == 1
	}, 3*time.Second, 10*time.Millisecond, "message A never started after admission degradation")
	runs, _ := env.store.ActiveRuns(context.Background(), []string{"sess-1"})
	require.Empty(t, runs["sess-1"], "degraded turn must not hold a claim")

	// Message B degrades the same way: immediate unguarded start, no queue
	// entry, no claim churn.
	wB := env.postMessageAsync("sess-1", "message B")
	require.Eventually(t, func() bool {
		return env.stub.started.Load() == 2
	}, 3*time.Second, 10*time.Millisecond, "message B never started after admission degradation")
	n, _ := env.store.QueueLen(context.Background(), "sess-1")
	require.Zero(t, n, "degraded admission must not leave a queue entry")

	close(env.stub.release)
	select {
	case <-wA:
	case <-time.After(5 * time.Second):
		t.Fatal("degraded turn A never completed")
	}
	select {
	case <-wB:
	case <-time.After(5 * time.Second):
		t.Fatal("degraded turn B never completed")
	}
}

// failingAdmitStore wraps the MemStore with an always-failing AdmitMessage
// (queue transport degradation).
type failingAdmitStore struct {
	*run.MemStore
}

func (f *failingAdmitStore) AdmitMessage(ctx context.Context, sessionID string, entry run.QueueEntry, maxDepth int) (run.AdmitResult, error) {
	return run.AdmitResult{}, errors.New("admission transport unavailable")
}

// TestQueue_Continuation (design D5 + tasks 6.1/6.5): after turn A's
// terminal persistence and claim release, the queued head B starts WITHOUT
// a new HTTP request; B's SSE stream (the original POST B connection)
// carries queued → agent_start → … → done in order; the queue drains; the
// claim is released at the end; and the persisted history keeps B's user
// row strictly after every row of turn A (D4: pump-time msg_time).
func TestQueue_Continuation(t *testing.T) {
	stub := newQueueStub()
	env := newQueueEnv(t, stub, 8)

	wA := env.postMessageAsync("sess-1", "message A")
	env.awaitClaim(t, true)
	wB := env.postMessageAsync("sess-1", "message B")
	env.awaitQueueDepth(t, 1)

	// Release A: A finalizes, the pump starts B, B runs to completion on
	// the already-closed release gate.
	close(stub.release)

	select {
	case rec := <-wA:
		require.Equal(t, http.StatusOK, rec.Code)
		body := rec.Body.String()
		require.Contains(t, body, "event: agent_start")
		require.Contains(t, body, "event: done")
	case <-time.After(5 * time.Second):
		t.Fatal("turn A never completed")
	}
	var recB *httptest.ResponseRecorder
	select {
	case recB = <-wB:
	case <-time.After(5 * time.Second):
		t.Fatal("queued turn B never completed")
	}
	require.Equal(t, http.StatusOK, recB.Code, "body: %s", recB.Body.String())
	bodyB := recB.Body.String()
	require.Equal(t, 1, strings.Count(bodyB, "event: queued"))
	require.Equal(t, 1, strings.Count(bodyB, "event: agent_start"))
	require.Equal(t, 1, strings.Count(bodyB, "event: done"))
	require.Less(t, strings.Index(bodyB, "event: queued"), strings.Index(bodyB, "event: agent_start"),
		"queued event must precede the turn's events on the same stream")
	require.Equal(t, "trace-2", recB.Header().Get("X-Run-Id"))

	// The pumped run's meta lifecycle: queued (set at enqueue) → running
	// (SetStatus at pump) → done (terminal bookkeeping) — one run id
	// throughout, no re-initialization.
	metaB, ok, _ := env.store.GetMeta(context.Background(), "trace-2")
	require.True(t, ok)
	require.Equal(t, run.StatusDone, metaB.Status, "pumped run did not finalize as done")
	require.Equal(t, "sess-1", metaB.SessionID)

	// The queue drained and the claim is free again.
	env.awaitQueueDepth(t, 0)
	require.Eventually(t, func() bool {
		runs, _ := env.store.ActiveRuns(context.Background(), []string{"sess-1"})
		return runs["sess-1"] == ""
	}, 2*time.Second, 5*time.Millisecond, "claim not released after the queued turn finished")

	// History (design D4): the queued user row landed at PUMP time — in the
	// dual-write cache (the send-time user row's landing spot) strictly after
	// A's user row, and after every row turn A's terminal batch persisted
	// (those batches precede B's start by the pump's post-Finalize position).
	dual := env.redis.dualSnapshot()
	var userA, userB *model.Message
	for i := range dual {
		if dual[i].Role == model.RoleUser && strings.Contains(dual[i].Content, "message A") {
			userA = &dual[i]
		}
		if dual[i].Role == model.RoleUser && strings.Contains(dual[i].Content, "message B") {
			userB = &dual[i]
		}
	}
	require.NotNil(t, userA, "turn A's user row missing from the send-time cache")
	require.NotNil(t, userB, "queued message never landed in the send-time cache at pump time")
	require.True(t, userB.MsgTime.After(userA.MsgTime), "queued user row dated at/before the previous turn's user row")

	// Turn A's terminal batch (the FIRST persisted batch — A's terminal
	// persistence strictly precedes B's pump) must predate B's pump-time
	// user row. B's own terminal batch naturally follows its user row and is
	// not part of the comparison.
	batches, _ := env.mysql.finalBatchSnapshots()
	require.NotEmpty(t, batches, "turn A never persisted its terminal batch")
	for i := range batches[0] {
		require.False(t, userB.MsgTime.Before(batches[0][i].MsgTime),
			"queued user row (%s) sorted before turn A's persisted row (%s)", userB.MsgTime, batches[0][i].MsgTime)
	}

}

// TestQueue_ShutdownGateSkipsPump (design D5, task 6.3): with the shutdown
// gate tripped, the terminal path skips the pump — the entry stays queued
// for the post-restart reconciler.
func TestQueue_ShutdownGateSkipsPump(t *testing.T) {
	stub := newQueueStub()
	env := newQueueEnv(t, stub, 8)

	wA := env.postMessageAsync("sess-1", "message A")
	env.awaitClaim(t, true)
	env.postMessageAsync("sess-1", "message B")
	env.awaitQueueDepth(t, 1)

	// Trip the process-level gate, then end turn A.
	env.streamH.StopNewTurns()
	close(stub.release)
	select {
	case <-wA:
	case <-time.After(5 * time.Second):
		t.Fatal("turn A never completed")
	}

	// A finalized and released, but no pump fired: B stays queued.
	require.Eventually(t, func() bool {
		runs, _ := env.store.ActiveRuns(context.Background(), []string{"sess-1"})
		return runs["sess-1"] == ""
	}, 2*time.Second, 5*time.Millisecond, "claim not released after A")
	n, _ := env.store.QueueLen(context.Background(), "sess-1")
	require.Equal(t, 1, n, "shutdown gate must leave the queued entry enqueued")
	meta, ok, _ := env.store.GetMeta(context.Background(), "trace-2")
	require.True(t, ok)
	require.Equal(t, run.StatusQueued, meta.Status, "queued run must not be started during shutdown")
}
