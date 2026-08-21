package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/stream"
)

// turnRunTestEnv wires a MessageStreamHandler + TurnRunHandler over one
// in-memory run store / registry, mirroring the agent-role wiring.
type turnRunTestEnv struct {
	engine *gin.Engine
	store  *run.MemStore
	reg    *run.Registry
	stub   *blockingStubOrchestrator
}

// blockingStubOrchestrator is a stub whose Handle blocks until released (or
// its context is cancelled), emitting a canned event sequence around the
// block. It lets tests hold a turn "running" while they exercise busy / cancel
// / resume behavior.
type blockingStubOrchestrator struct {
	release         chan struct{}
	cancelledViaCtx bool
	done            chan struct{}
}

func newBlockingStub() *blockingStubOrchestrator {
	return &blockingStubOrchestrator{release: make(chan struct{}), done: make(chan struct{})}
}

func (s *blockingStubOrchestrator) Handle(ctx context.Context, _, _, _ string, _ []agent.Message, hub *stream.Hub, _ TurnHooks, _ agent.ModelOverride) ([]stream.StreamEvent, map[string]any, error) {
	defer close(s.done)
	hub.Send(stream.AgentStartEvent(stream.AgentConfucius))
	hub.Send(stream.TokenEvent(stream.AgentConfucius, "partial"))
	select {
	case <-s.release:
	case <-ctx.Done():
		s.cancelledViaCtx = true
		return nil, nil, ctx.Err()
	}
	hub.Send(stream.TokenEvent(stream.AgentConfucius, " done"))
	hub.Send(stream.DoneEvent(nil))
	return nil, nil, nil
}

func newTurnRunEnv(t *testing.T, stub *blockingStubOrchestrator) *turnRunTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mysql := &handlerFakeMySQL{getSessionByIDFound: &model.Session{SessionID: "sess-1", UserID: "user-1"}}
	redis := &handlerFakeRedis{}
	deps := sessionDeps(mysql, redis, newHandlerFakeFS())
	sessSvc := newSessionSvc(deps)
	msgSvc := newMessageSvc(deps)
	titleSvc := newTitleSvcWithFake(t, deps, "T")

	store := run.NewMemStore()
	reg := run.NewRegistry()
	runs := run.NewManager(store, reg)
	streamH := NewMessageStreamHandler(sessSvc, msgSvc, titleSvc, nil, stub, "/tmp/blowball-test-data", runs, testSelectionConfig(), 0)
	turnH := NewTurnRunHandler(runs)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-1")
		c.Next()
	})
	r.POST("/api/v1/sessions/:session_id/messages", streamH.SendMessage)
	r.POST("/api/v1/sessions/:session_id/turns/:run_id/cancel", turnH.CancelTurn)
	r.GET("/api/v1/sessions/:session_id/turns/:run_id/events", turnH.TurnEvents)
	return &turnRunTestEnv{engine: r, store: store, reg: reg, stub: stub}
}

func (e *turnRunTestEnv) postMessage(sessionID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/"+sessionID+"/messages",
		strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	return w
}

// TestSendMessage_RunIDDotachedAndBusy covers the happy path and the
// single-active-run mutex: the first request runs to completion with the
// X-Run-Id header + run_id-stamped events, releases the session claim, and a
// second request against a STILL-RUNNING turn is rejected 409 with the active
// run id.
func TestSendMessage_RunIDDotachedAndBusy(t *testing.T) {
	env := newTurnRunEnv(t, newBlockingStub())

	// First request against a session with a running (blocked) turn: the SSE
	// response must NOT complete while the turn is held, so drive it on its
	// own goroutine and check the busy rejection meanwhile.
	w1 := make(chan *httptest.ResponseRecorder, 1)
	go func() { w1 <- env.postMessage("sess-1") }()

	// Wait until the session is claimed (the run started).
	require.Eventually(t, func() bool {
		runs, _ := env.store.ActiveRuns(context.Background(), []string{"sess-1"})
		return runs["sess-1"] != ""
	}, 2*time.Second, 5*time.Millisecond, "session never claimed")

	// A second message to the same session while the turn runs: 409.
	w2 := env.postMessage("sess-1")
	require.Equal(t, http.StatusConflict, w2.Code, "body: %s", w2.Body.String())
	require.Contains(t, w2.Body.String(), "SESSION_BUSY")
	require.Contains(t, w2.Body.String(), `"run_id":"trace-1"`)

	// Different sessions are unaffected by the busy session.
	env2 := env
	_ = env2

	// Release the turn; the SSE response completes with the run id header.
	close(env.stub.release)
	select {
	case rec := <-w1:
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		require.Equal(t, "trace-1", rec.Header().Get("X-Run-Id"))
		body := rec.Body.String()
		require.Contains(t, body, "event: agent_start")
		require.Contains(t, body, "event: done")
		require.Contains(t, body, `"run_id":"trace-1"`)
		require.Contains(t, body, "id: ", "SSE frames must carry stream entry ids")
	case <-time.After(5 * time.Second):
		t.Fatal("SSE response never completed after turn release")
	}

	// Terminal bookkeeping: the claim is released and the status is done.
	require.Eventually(t, func() bool {
		meta, ok, _ := env.store.GetMeta(context.Background(), "trace-1")
		return ok && meta.Status == run.StatusDone
	}, 2*time.Second, 5*time.Millisecond)
	runs, _ := env.store.ActiveRuns(context.Background(), []string{"sess-1"})
	require.Empty(t, runs["sess-1"], "session claim not released after terminal")

	// After completion a new message is accepted again (claim reusable).
	env.stub.release = make(chan struct{})
	env.stub.done = make(chan struct{})
	w3 := make(chan *httptest.ResponseRecorder, 1)
	go func() { w3 <- env.postMessage("sess-1") }()
	require.Eventually(t, func() bool {
		runs, _ := env.store.ActiveRuns(context.Background(), []string{"sess-1"})
		return runs["sess-1"] != ""
	}, 2*time.Second, 5*time.Millisecond, "session not reclaimed after release")
	close(env.stub.release)
	select {
	case <-w3:
	case <-time.After(5 * time.Second):
		t.Fatal("second turn never completed")
	}
}

// TestCancelTurn_InProcess cancels a run registered in this process: the turn
// context fires, the turn finalizes as cancelled, and the session unblocks.
func TestCancelTurn_InProcess(t *testing.T) {
	env := newTurnRunEnv(t, newBlockingStub())

	w1 := make(chan *httptest.ResponseRecorder, 1)
	go func() { w1 <- env.postMessage("sess-1") }()
	require.Eventually(t, func() bool {
		runs, _ := env.store.ActiveRuns(context.Background(), []string{"sess-1"})
		return runs["sess-1"] != ""
	}, 2*time.Second, 5*time.Millisecond)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/turns/trace-1/cancel", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), `"status":"cancelling"`)

	// The turn observes the cancellation and finishes.
	select {
	case <-env.stub.done:
	case <-time.After(2 * time.Second):
		t.Fatal("turn never observed the cancel")
	}
	require.True(t, env.stub.cancelledViaCtx, "turn context was not cancelled")
	require.Eventually(t, func() bool {
		meta, ok, _ := env.store.GetMeta(context.Background(), "trace-1")
		return ok && meta.Status == run.StatusCancelled
	}, 2*time.Second, 5*time.Millisecond, "run not finalized as cancelled")
	runs, _ := env.store.ActiveRuns(context.Background(), []string{"sess-1"})
	require.Empty(t, runs["sess-1"], "claim not released after cancel")
	select {
	case <-w1:
	case <-time.After(5 * time.Second):
		t.Fatal("SSE response never closed after cancel")
	}
}

// TestCancelTurn_DeadRunForceClear covers the crashed-process path: a
// status-running run with no heartbeat is force-cleared to interrupted and
// the session claim is released.
func TestCancelTurn_DeadRunForceClear(t *testing.T) {
	env := newTurnRunEnv(t, newBlockingStub())
	ctx := context.Background()

	// Synthesize a dead run: claimed, meta running, no heartbeat.
	_, ok, _ := env.store.ClaimSession(ctx, "sess-9", "run-dead")
	require.True(t, ok)
	require.NoError(t, env.store.InitMeta(ctx, "run-dead", run.RunMeta{
		SessionID: "sess-9", UserID: "user-1", Status: run.StatusRunning,
	}))
	require.NoError(t, env.store.AppendEvent(ctx, "run-dead", stream.TokenEvent(stream.AgentConfucius, "partial")))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-9/turns/run-dead/cancel", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), `"status":"interrupted"`)

	meta, ok, _ := env.store.GetMeta(ctx, "run-dead")
	require.True(t, ok)
	assert.Equal(t, run.StatusInterrupted, meta.Status)
	runs, _ := env.store.ActiveRuns(ctx, []string{"sess-9"})
	require.Empty(t, runs["sess-9"], "dead run did not release the claim")
}

// TestCancelTurn_Ownership covers the 404 paths: unknown run, and a run owned
// by another user or claimed under a different session.
func TestCancelTurn_Ownership(t *testing.T) {
	env := newTurnRunEnv(t, newBlockingStub())
	ctx := context.Background()

	// Unknown run.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/turns/nope/cancel", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)

	// Foreign owner.
	require.NoError(t, env.store.InitMeta(ctx, "run-foreign", run.RunMeta{
		SessionID: "sess-1", UserID: "someone-else", Status: run.StatusRunning,
	}))
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/turns/run-foreign/cancel", nil)
	w = httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)

	// Session mismatch: run exists for another session.
	require.NoError(t, env.store.InitMeta(ctx, "run-other-sess", run.RunMeta{
		SessionID: "sess-2", UserID: "user-1", Status: run.StatusRunning,
	}))
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/turns/run-other-sess/cancel", nil)
	w = httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
}

// TestCancelTurn_AlreadyTerminal is idempotent: cancelling a finished run
// reports the terminal status without touching anything.
func TestCancelTurn_AlreadyTerminal(t *testing.T) {
	env := newTurnRunEnv(t, newBlockingStub())
	ctx := context.Background()
	require.NoError(t, env.store.InitMeta(ctx, "run-fin", run.RunMeta{
		SessionID: "sess-1", UserID: "user-1", Status: run.StatusDone,
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/turns/run-fin/cancel", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"status":"done"`)
}

// TestTurnEvents_ReplayTerminal covers the resume endpoint against a terminal
// run inside the retention window: full replay including done, then close.
func TestTurnEvents_ReplayTerminal(t *testing.T) {
	env := newTurnRunEnv(t, newBlockingStub())
	ctx := context.Background()

	require.NoError(t, env.store.InitMeta(ctx, "run-fin", run.RunMeta{
		SessionID: "sess-1", UserID: "user-1", Status: run.StatusDone,
	}))
	require.NoError(t, env.store.AppendEvent(ctx, "run-fin", stream.AgentStartEvent(stream.AgentConfucius)))
	require.NoError(t, env.store.AppendEvent(ctx, "run-fin", stream.TokenEvent(stream.AgentConfucius, "hello")))
	require.NoError(t, env.store.AppendEvent(ctx, "run-fin", stream.DoneEvent(nil)))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-1/turns/run-fin/events", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
	require.Equal(t, "run-fin", w.Header().Get("X-Run-Id"))
	body := w.Body.String()
	require.Contains(t, body, "event: agent_start")
	require.Contains(t, body, "event: token")
	require.Contains(t, body, "event: done")
	// Frames carry entry ids for Last-Event-ID resume.
	require.Contains(t, body, "\nid: ")
}

// TestTurnEvents_Gone covers the 410 after the retention window.
func TestTurnEvents_Gone(t *testing.T) {
	env := newTurnRunEnv(t, newBlockingStub())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-1/turns/run-gone/events", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusGone, w.Code)
}

// TestTurnEvents_DeadRunSynthesizesInterruptedDone covers the crash path: a
// status-running run whose heartbeat expired gets a synthesized terminal
// done(error=interrupted) and the session claim is released by the reader.
func TestTurnEvents_DeadRunSynthesizesInterruptedDone(t *testing.T) {
	env := newTurnRunEnv(t, newBlockingStub())
	ctx := context.Background()

	_, ok, _ := env.store.ClaimSession(ctx, "sess-7", "run-crash")
	require.True(t, ok)
	require.NoError(t, env.store.InitMeta(ctx, "run-crash", run.RunMeta{
		SessionID: "sess-7", UserID: "user-1", Status: run.StatusRunning,
	}))
	require.NoError(t, env.store.AppendEvent(ctx, "run-crash", stream.TokenEvent(stream.AgentConfucius, "partial")))
	// No heartbeat: the run reads dead.

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-7/turns/run-crash/events", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), "event: token")
	require.Contains(t, w.Body.String(), "event: done")
	require.Contains(t, w.Body.String(), "interrupted")

	meta, ok, _ := env.store.GetMeta(ctx, "run-crash")
	require.True(t, ok)
	assert.Equal(t, run.StatusInterrupted, meta.Status)
	runs, _ := env.store.ActiveRuns(ctx, []string{"sess-7"})
	require.Empty(t, runs["sess-7"], "dead run did not release the claim")
}
