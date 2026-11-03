package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/store/fs"
	mysqlstore "github.com/lush/blowball/internal/store/mysql"
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
	// AdmitMessage publishes the meta (queued) and the queued event atomically.
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

	// Give the subscriber time to replay and poll. The recorder is owned by
	// the serving goroutine while the stream is open (reading it here would
	// race), so the queued-state assertions are behavioral: a dead-run
	// misclassification would synthesize an interrupted done AND close the
	// stream — the still-open check below catches exactly that.
	time.Sleep(300 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("subscriber closed a queued run's stream (should keep tailing — the dead-run ladder misclassified it)")
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
	sessSvc *service.SessionService
	mysql   *handlerFakeMySQL
	redis   *handlerFakeRedis
	atts    *fakeAttachmentStore
	dataDir string
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
	atts := &fakeAttachmentStore{rows: map[string]model.Attachment{}}
	dataDir := t.TempDir()
	streamH := NewMessageStreamHandler(sessSvc, msgSvc, titleSvc, nil, nil, stub, dataDir, runs, testSelectionConfig(), 0).
		WithAttachmentStore(atts)
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
	r.GET("/api/v1/sessions/:session_id/queue", handlerQueueRoute(sessSvc, runs))
	return &queueEnv{engine: r, store: store, reg: reg, stub: stub, streamH: streamH, msgSvc: msgSvc, sessSvc: sessSvc, mysql: mysql, redis: redis, atts: atts, dataDir: dataDir, next: next}
}

func handlerQueueRoute(sessSvc *service.SessionService, runs *run.Manager) gin.HandlerFunc {
	return NewQueueHandler(sessSvc, runs).ListQueue
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

// ── queue read API, reconciler, and session-state tests ────────────────────

// TestQueueRead_ListQueue: the queue read endpoint returns the entries in
// enqueue order with the snapshot fields, and 404s foreign/missing sessions
// without disclosing them.
func TestQueueRead_ListQueue(t *testing.T) {
	env := newQueueEnv(t, newQueueStub(), 8)
	ctx := context.Background()

	// Seed: a held claim plus two queued entries behind it.
	_, _, err := env.store.ClaimSession(ctx, "sess-1", "run-a")
	require.NoError(t, err)
	for i, id := range []string{"trace-2", "trace-3"} {
		_, err := env.store.AdmitMessage(ctx, "sess-1", run.QueueEntry{
			RunID: id, SessionID: "sess-1", UserID: "user-1",
			Content: "msg " + id, Model: "test-model",
			EnqueuedAt: time.Unix(1700000000+int64(i), 0).UTC(),
		}, 8)
		require.NoError(t, err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-1/queue", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Queue []struct {
			RunID      string `json:"run_id"`
			Content    string `json:"content"`
			Model      string `json:"model"`
			EnqueuedAt string `json:"enqueued_at"`
		} `json:"queue"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Queue, 2)
	require.Equal(t, "trace-2", resp.Queue[0].RunID)
	require.Equal(t, "msg trace-2", resp.Queue[0].Content)
	require.Equal(t, "test-model", resp.Queue[0].Model)
	require.Equal(t, "trace-3", resp.Queue[1].RunID, "entries must be in enqueue order")

	// Wrong owner: 404, existence not disclosed.
	env.mysql.mu.Lock()
	env.mysql.getSessionByIDFound = &model.Session{SessionID: "sess-1", UserID: "user-2"}
	env.mysql.mu.Unlock()
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-1/queue", nil)
	w2 := httptest.NewRecorder()
	env.engine.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusNotFound, w2.Code)
	require.Contains(t, w2.Body.String(), "NOT_FOUND")
}

// TestQueueReconciler_ContinuesCrashResidue (task 7.3): a queue left behind
// by a process that died between claim release and pump is continued by the
// reconciler within one cycle — same pump path, same turn semantics.
func TestQueueReconciler_ContinuesCrashResidue(t *testing.T) {
	env := newQueueEnv(t, newQueueStub(), 8)
	ctx := context.Background()

	// Crash residue: run-a claimed, B admitted behind it, then run-a's claim
	// expired/released without any pump running (the owning process died).
	_, _, err := env.store.ClaimSession(ctx, "sess-1", "run-a")
	require.NoError(t, err)
	entry := run.QueueEntry{RunID: "trace-2", SessionID: "sess-1", UserID: "user-1",
		Content: "queued B", Model: "test-model", EnqueuedAt: time.Unix(1700000000, 0).UTC()}
	_, err = env.store.AdmitMessage(ctx, "sess-1", entry, 8)
	require.NoError(t, err)
	require.NoError(t, env.store.ReleaseSession(ctx, "sess-1", "run-a"))

	// B's turn can run to completion immediately (release gate pre-closed).
	close(env.stub.release)

	reconciler := NewQueueReconciler(env.streamH, 10*time.Millisecond)
	reconciler.Start()
	defer reconciler.Close()

	require.Eventually(t, func() bool {
		return env.stub.started.Load() == 1
	}, 2*time.Second, 10*time.Millisecond, "reconciler never continued the crashed residue")
	require.Eventually(t, func() bool {
		n, _ := env.store.QueueLen(ctx, "sess-1")
		return n == 0
	}, 2*time.Second, 10*time.Millisecond, "queue never drained")
	require.Eventually(t, func() bool {
		meta, ok, _ := env.store.GetMeta(ctx, "trace-2")
		return ok && meta.Status == run.StatusDone
	}, 3*time.Second, 10*time.Millisecond, "reconciled run never finalized")
}

// TestQueueReconciler_RespectsShutdownGate: with the gate tripped, the
// reconciler leaves entries enqueued (the post-restart contract).
func TestQueueReconciler_RespectsShutdownGate(t *testing.T) {
	env := newQueueEnv(t, newQueueStub(), 8)
	ctx := context.Background()

	_, _, err := env.store.ClaimSession(ctx, "sess-1", "run-a")
	require.NoError(t, err)
	_, err = env.store.AdmitMessage(ctx, "sess-1", run.QueueEntry{
		RunID: "trace-2", SessionID: "sess-1", UserID: "user-1", Content: "queued B",
	}, 8)
	require.NoError(t, err)
	require.NoError(t, env.store.ReleaseSession(ctx, "sess-1", "run-a"))

	env.streamH.StopNewTurns()
	reconciler := NewQueueReconciler(env.streamH, 10*time.Millisecond)
	reconciler.Start()
	defer reconciler.Close()

	// A few cycles pass; nothing starts.
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, env.stub.started.Load(), "reconciler started a turn while the shutdown gate was tripped")
	n, _ := env.store.QueueLen(ctx, "sess-1")
	require.Equal(t, 1, n)
}

// TestSessionQueuedCount (task 8.2): with queue info enabled, list and
// detail carry queued_count for non-empty queues and omit it otherwise;
// disabled keeps the fields absent (additive contract).
func TestSessionQueuedCount(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	ctx := context.Background()

	store := run.NewMemStore()
	// s-1 has a running claim + two queued entries; s-2 is idle.
	_, _, err := store.ClaimSession(ctx, "s-1", "run-9")
	require.NoError(t, err)
	for _, id := range []string{"q-1", "q-2"} {
		_, err := store.AdmitMessage(ctx, "s-1", run.QueueEntry{RunID: id, SessionID: "s-1"}, 8)
		require.NoError(t, err)
	}

	env.mysql.mu.Lock()
	env.mysql.listSessionsRows = []mysqlstore.SessionWithTitle{
		{SessionID: "s-1", UserID: "user-1", Title: "Alpha", UpdateTime: time.Unix(1_700_000_010, 0).UTC()},
		{SessionID: "s-2", UserID: "user-1", Title: "Beta", UpdateTime: time.Unix(1_700_000_005, 0).UTC()},
	}
	env.mysql.getSessionByIDFound = &model.Session{SessionID: "s-1", UserID: "user-1"}
	env.mysql.mu.Unlock()

	buildEngine := func(queueInfo bool) *gin.Engine {
		h := NewSessionHandler(
			newSessionSvc(sessionDeps(env.mysql, env.redis, env.fs)),
			service.NewTitleService(nil, env.mysql, config.OpenAIConfig{TitleModel: "title-model"}),
			store,
		)
		if queueInfo {
			h.EnableQueueInfo()
		}
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set(middleware.UserIDKey, "user-1")
			c.Set(middleware.TraceIDKey, "trace-1")
			c.Next()
		})
		r.GET("/api/v1/sessions", h.ListSessions)
		r.GET("/api/v1/sessions/:session_id", h.GetSession)
		return r
	}

	// Enabled: queued_count present on s-1 only.
	w := httptest.NewRecorder()
	buildEngine(true).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	var listResp struct {
		Sessions []struct {
			SessionID   string `json:"session_id"`
			QueuedCount *int   `json:"queued_count"`
		} `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listResp))
	require.Len(t, listResp.Sessions, 2)
	require.NotNil(t, listResp.Sessions[0].QueuedCount)
	require.Equal(t, 2, *listResp.Sessions[0].QueuedCount)
	require.Nil(t, listResp.Sessions[1].QueuedCount, "idle session must omit queued_count")

	w = httptest.NewRecorder()
	buildEngine(true).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/s-1", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"queued_count":2`, "body: %s", w.Body.String())

	// Disabled (default): the field is absent — additive contract.
	w = httptest.NewRecorder()
	buildEngine(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	require.NotContains(t, w.Body.String(), "queued_count", "disabled handler must not emit queued_count")
}

// TestDeleteSession_ClearsQueue (task 8.3): session deletion invokes the
// registered queue clearer with the session id (entries are transient
// snapshots, never archived); an unwired service skips it entirely.
func TestDeleteSession_ClearsQueue(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	ctx := context.Background()

	var cleared []string
	sessSvc := newSessionSvc(sessionDeps(env.mysql, env.redis, env.fs)).
		WithQueueClearer(func(_ context.Context, sessionID string) error {
			cleared = append(cleared, sessionID)
			return nil
		})
	env.mysql.mu.Lock()
	env.mysql.getSessionByIDFound = &model.Session{SessionID: "sess-1", UserID: "user-1"}
	env.mysql.mu.Unlock()

	require.NoError(t, sessSvc.DeleteSession(ctx, "user-1", "sess-1"))
	require.Equal(t, []string{"sess-1"}, cleared, "delete did not clear the session queue")

	// Unwired (the default pre-queue wiring): no queue interaction at all —
	// and the delete itself still succeeds.
	bare := newSessionSvc(sessionDeps(env.mysql, env.redis, env.fs))
	env.mysql.mu.Lock()
	env.mysql.getSessionByIDFound = &model.Session{SessionID: "sess-2", UserID: "user-1"}
	env.mysql.mu.Unlock()
	require.NoError(t, bare.DeleteSession(ctx, "user-1", "sess-2"))
	require.Len(t, cleared, 1, "unwired service must not touch the queue")
}

// ── review-fix behavior tests ──────────────────────────────────────────────

// recordingStore wraps the MemStore, recording the lifecycle call sequence
// (method + run id) so tests can pin ORDERING guarantees — the pre-publish
// heartbeat before the running status, and the reconciler's rotation
// coverage.
type recordingStore struct {
	*run.MemStore
	mu    sync.Mutex
	calls []string
}

func newRecordingStore() *recordingStore { return &recordingStore{MemStore: run.NewMemStore()} }

func (r *recordingStore) record(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}

func (r *recordingStore) Heartbeat(ctx context.Context, runID, sessionID string) error {
	r.record("Heartbeat:%s", runID)
	return r.MemStore.Heartbeat(ctx, runID, sessionID)
}

func (r *recordingStore) SetStatus(ctx context.Context, runID, status string) error {
	r.record("SetStatus:%s:%s", runID, status)
	return r.MemStore.SetStatus(ctx, runID, status)
}

func (r *recordingStore) InitMeta(ctx context.Context, runID string, m run.RunMeta) error {
	r.record("InitMeta:%s:%s", runID, m.Status)
	return r.MemStore.InitMeta(ctx, runID, m)
}

func (r *recordingStore) TouchQueued(ctx context.Context, runID string) error {
	r.record("TouchQueued:%s", runID)
	return r.MemStore.TouchQueued(ctx, runID)
}

func (r *recordingStore) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// firstIndex returns the index of the first recorded call matching prefix,
// or -1.
func firstIndex(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// TestQueue_HeartbeatPrecedesRunningStatus pins the review fix 1 ordering:
// for BOTH turn start paths (fresh HTTP admission and the queue pump), the
// first Heartbeat precedes the running publication. A queued POST's SSE
// subscriber is already tailing; if running were observable before the alive
// key existed, the subscriber's dead-run ladder would force-clear the claim
// while the turn is still starting.
func TestQueue_HeartbeatPrecedesRunningStatus(t *testing.T) {
	store := newRecordingStore()
	env := newQueueEnvWithStore(t, newQueueStub(), 8, store)

	// Fresh HTTP admission (trace-1) and a pumped queued turn (trace-2).
	wA := env.postMessageAsync("sess-1", "message A")
	env.awaitClaim(t, true)
	wB := env.postMessageAsync("sess-1", "message B")
	env.awaitQueueDepth(t, 1)
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

	calls := store.snapshot()
	for _, tc := range []struct{ runID, statusCall string }{
		{"trace-1", "InitMeta:trace-1:running"},
		{"trace-2", "SetStatus:trace-2:running"},
	} {
		beat := firstIndex(calls, "Heartbeat:"+tc.runID)
		publish := firstIndex(calls, tc.statusCall)
		require.NotEqual(t, -1, beat, "%s: no heartbeat recorded", tc.runID)
		require.NotEqual(t, -1, publish, "%s: no running publication recorded", tc.runID)
		require.Less(t, beat, publish,
			"%s: running published (call #%d) before the first heartbeat (call #%d) — the dead-run ladder window is open",
			tc.runID, publish, beat)
	}
}

// TestQueue_MetaRebuildAfterExpiry (review fix 5): a queued entry whose meta
// expired under a long wait (crashed reconciler, queue list has no TTL) is
// rebuilt with the FULL RunMeta at pump time — not a status-only stub — so
// the cancel and events endpoints' ownership checks keep working.
func TestQueue_MetaRebuildAfterExpiry(t *testing.T) {
	env := newQueueEnv(t, newQueueStub(), 8)
	ctx := context.Background()

	// Seed: a crashed run held the claim, B queued behind it, then the claim
	// was released without a pump.
	_, _, err := env.store.ClaimSession(ctx, "sess-1", "run-a")
	require.NoError(t, err)
	entry := run.QueueEntry{RunID: "trace-2", SessionID: "sess-1", UserID: "user-1",
		Content: "queued B", Model: "test-model",
		Selection:  run.QueueSelection{Model: "test-model", MaxContextTokens: 128000, MaxCompletionTokens: 8192},
		EnqueuedAt: time.Unix(1700000000, 0).UTC()}
	_, err = env.store.AdmitMessage(ctx, "sess-1", entry, 8)
	require.NoError(t, err)
	require.NoError(t, env.store.ReleaseSession(ctx, "sess-1", "run-a"))

	// Expire the run-key family: the queue list survives (no TTL), the meta
	// does not.
	mem := env.store.(*run.MemStore)
	origNow := mem.Now
	mem.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	_, ok, err := env.store.GetMeta(ctx, "trace-2") // triggers the lazy expiry
	require.NoError(t, err)
	require.False(t, ok, "meta should have expired")
	mem.Now = origNow

	// The reconciler continues the residue; the pumped turn rebuilds the
	// FULL meta from the entry snapshot.
	close(env.stub.release)
	reconciler := NewQueueReconciler(env.streamH, 10*time.Millisecond)
	reconciler.Start()
	defer reconciler.Close()
	require.Eventually(t, func() bool {
		meta, ok, _ := env.store.GetMeta(ctx, "trace-2")
		return ok && run.Terminal(meta.Status)
	}, 3*time.Second, 10*time.Millisecond, "reconciled run never finalized")

	meta, ok, err := env.store.GetMeta(ctx, "trace-2")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, run.StatusDone, meta.Status)
	// The rebuilt meta carries the full ownership fields — a status-only
	// stub would leave the cancel/events ownership checks 404-ing.
	require.Equal(t, "sess-1", meta.SessionID)
	require.Equal(t, "user-1", meta.UserID)
	require.Equal(t, "test-model", meta.Model)
	require.Equal(t, entry.EnqueuedAt.Format(time.RFC3339), meta.CreatedAt,
		"the rebuild restores the enqueue-time creation timestamp")
}

// TestQueueReconciler_RotatesBeyondCap (review fix 4): with more queued
// sessions than one cycle's cap, the handling window ROTATES — the tail
// sessions get their TouchQueued keep-alive within later cycles instead of
// being starved by a stable scan order.
func TestQueueReconciler_RotatesBeyondCap(t *testing.T) {
	store := newRecordingStore()
	env := newQueueEnvWithStore(t, newQueueStub(), 8, store)
	ctx := context.Background()

	// Five sessions, each with a HELD claim (a long running turn) and one
	// queued entry — nothing is pumpable, so the reconciler's only visible
	// effect is the TTL keep-alive (TouchQueued), which the recorder tracks.
	const sessions = 5
	ids := make([]string, 0, sessions)
	for i := 0; i < sessions; i++ {
		sid := fmt.Sprintf("sess-%d", i)
		ids = append(ids, sid)
		rid := fmt.Sprintf("trace-%d", i)
		_, _, err := store.ClaimSession(ctx, sid, "run-holder-"+rid)
		require.NoError(t, err)
		_, err = store.AdmitMessage(ctx, sid, run.QueueEntry{
			RunID: rid, SessionID: sid, UserID: "user-1", Content: "q", Model: "test-model",
		}, 8)
		require.NoError(t, err)
	}

	reconciler := NewQueueReconciler(env.streamH, 10*time.Millisecond)
	reconciler.maxSessions = 2
	reconciler.Start()
	defer reconciler.Close()

	// With cap 2 and five sorted sessions, cycles 1-2 cover traces 0-3 and
	// cycle 3 rotates onto the tail — the discriminating contract: EVERY
	// queued run is covered within a few cycles. (Without the rotation the
	// stable scan order would re-touch only traces 0-1 forever and the tail
	// would starve — both its pump chances and its TTL keep-alive.)
	_ = ids
	require.Eventually(t, func() bool {
		for i := 0; i < sessions; i++ {
			if firstIndex(store.snapshot(), fmt.Sprintf("TouchQueued:trace-%d", i)) < 0 {
				return false
			}
		}
		return true
	}, 1*time.Second, 10*time.Millisecond, "the rotation never covered the tail sessions")
}

// ── review round 2 behavior tests ──────────────────────────────────────────

// countCalls counts recorded calls with the given prefix.
func countCalls(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// TestQueue_StartupHeartbeatSustained (review round 2, fix 1): the
// pre-publish beat alone covers only HeartbeatTTL (15s); the send-time
// persistence runs BEFORE the drainer starts beating. Holding the direct
// MySQL write blocks startTurn mid-startup — the heartbeat keeper must keep
// beating during that window (the drainer cannot be the source: startTurn
// has not reached it).
func TestQueue_StartupHeartbeatSustained(t *testing.T) {
	store := newRecordingStore()
	env := newQueueEnvWithStore(t, newQueueStub(), 8, store)

	// Hold the first direct write (turn A's send-time user row) so the
	// startup phase is observably long.
	entered := make(chan struct{}, 1)
	hold := make(chan struct{})
	env.mysql.setAppendMessagesHold(entered, hold)

	wA := env.postMessageAsync("sess-1", "message A")
	<-entered

	// At this point: the pre-publish beat fired, the meta is published
	// (running), and the persist is blocked — the drainer has NOT started.
	// The keeper must sustain the beat: after one full tick (5s) at least a
	// second Heartbeat for this run exists while the persist is still held.
	require.Eventually(t, func() bool {
		return countCalls(store.snapshot(), "Heartbeat:trace-1") >= 2
	}, 8*time.Second, 50*time.Millisecond,
		"no sustained heartbeat during the blocked startup persist — the queue SSE would misclassify the run as dead past 15s")

	// The alive key exists throughout (not expired) and the run is running.
	alive, err := env.store.Alive(context.Background(), "trace-1")
	require.NoError(t, err)
	require.True(t, alive, "alive key missing during the blocked startup persist")
	meta, ok, err := env.store.GetMeta(context.Background(), "trace-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, run.StatusRunning, meta.Status)

	close(hold)
	close(env.stub.release)
	select {
	case <-wA:
	case <-time.After(10 * time.Second):
		t.Fatal("turn A never completed after the hold released")
	}
}

// TestQueueCancel_SharedAttachmentKept (review round 2, fix 3): a queued
// message's newly-claimed attachment may be referenced by ANOTHER accepted
// queued message (same-session reuse is legal). Cancelling the first message
// must not roll the shared attachment back — only unreferenced ids are
// restored to staging.
func TestQueueCancel_SharedAttachmentKept(t *testing.T) {
	env := newQueueEnv(t, newQueueStub(), 8)
	ctx := context.Background()

	// Plant two staged attachments.
	stagingDir := filepath.Join(env.dataDir, "user-1", "attachments", fs.AttachmentStagingScope)
	require.NoError(t, os.MkdirAll(stagingDir, 0o755))
	for _, id := range []string{"att-shared", "att-solo"} {
		env.atts.rows[id] = model.Attachment{ID: id, UserID: "user-1", Filename: id + ".png", MIME: "image/png", Size: 4}
		require.NoError(t, os.WriteFile(filepath.Join(stagingDir, id+".png"), []byte("data"), 0o644))
	}

	// A holder claim so admissions queue.
	_, _, err := env.store.ClaimSession(ctx, "sess-1", "run-holder")
	require.NoError(t, err)

	// B enqueues first and claims both attachments into the session (the
	// bind the real send path performs before admission): files move into
	// the session dir, rows become session-bound.
	sessDir := filepath.Join(env.dataDir, "user-1", "attachments", "sess-1")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	for _, id := range []string{"att-shared", "att-solo"} {
		sid := "sess-1"
		env.atts.rows[id] = model.Attachment{ID: id, UserID: "user-1", SessionID: &sid, Filename: id + ".png", MIME: "image/png", Size: 4}
		require.NoError(t, os.Rename(filepath.Join(stagingDir, id+".png"), filepath.Join(sessDir, id+".png")))
	}
	ref := model.MessageAttachmentRef{ID: "att-shared", Filename: "att-shared.png", MIME: "image/png"}
	soloRef := model.MessageAttachmentRef{ID: "att-solo", Filename: "att-solo.png", MIME: "image/png"}
	_, err = env.store.AdmitMessage(ctx, "sess-1", run.QueueEntry{
		RunID: "trace-b", SessionID: "sess-1", UserID: "user-1", Content: "B",
		Attachments:  []model.MessageAttachmentRef{ref, soloRef},
		NewlyClaimed: []string{"att-shared", "att-solo"},
	}, 8)
	require.NoError(t, err)

	// C enqueues referencing the SHARED attachment (bound by B's claim —
	// legal same-session reuse; C's NewlyClaimed is empty).
	_, err = env.store.AdmitMessage(ctx, "sess-1", run.QueueEntry{
		RunID: "trace-c", SessionID: "sess-1", UserID: "user-1", Content: "C",
		Attachments: []model.MessageAttachmentRef{ref},
	}, 8)
	require.NoError(t, err)

	// Cancel B: the shared attachment must SURVIVE (C still references it);
	// the solo one rolls back.
	w := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/turns/trace-b/cancel", nil)
	w.Header.Set("Authorization", "Bearer x") // auth bypassed by the test middleware anyway
	rec := httptest.NewRecorder()
	env.engine.ServeHTTP(rec, w)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"status":"cancelled"`)

	shared := env.atts.rows["att-shared"]
	solo := env.atts.rows["att-solo"]
	require.NotNil(t, shared.SessionID, "shared attachment was unbound although another accepted message still references it")
	require.Equal(t, "sess-1", *shared.SessionID)
	require.Nil(t, solo.SessionID, "unreferenced attachment was not rolled back to staging")

	// Files follow the rows: shared stays in the session dir, solo returns
	// to staging.
	_, statErr := os.Stat(filepath.Join(sessDir, "att-shared.png"))
	require.NoError(t, statErr, "shared attachment file must stay bound for the surviving message")
	_, statErr = os.Stat(filepath.Join(stagingDir, "att-solo.png"))
	require.NoError(t, statErr, "solo attachment file must return to staging")

	// C is untouched by the cancel.
	n, err := env.store.QueueLen(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, 1, n)
}
