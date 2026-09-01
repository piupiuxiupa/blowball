package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/stream"
)

// This file covers harden-turn-persistence: the terminal message batch is
// durable (dual-write, or bounded retry) BEFORE Finalize releases the session
// claim, a heartbeat keeper spans the persist/drain phase, and the send-time
// at-least-once fallback for the user row actually triggers on a dual-tier
// failure.

// orderLog records cross-store event ordering (persist / drain / status /
// release / retire) so tests can assert the terminal sequence.
type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (l *orderLog) add(ev string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
}

func (l *orderLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.events))
	copy(out, l.events)
	return out
}

// recordingRunStore wraps a run.Store, recording the Finalize-relevant calls
// into an orderLog.
type recordingRunStore struct {
	run.Store
	log *orderLog
}

func (s *recordingRunStore) SetStatus(ctx context.Context, runID, status string) error {
	s.log.add("status")
	return s.Store.SetStatus(ctx, runID, status)
}

func (s *recordingRunStore) ReleaseSession(ctx context.Context, sessionID, runID string) error {
	s.log.add("release")
	return s.Store.ReleaseSession(ctx, sessionID, runID)
}

func (s *recordingRunStore) Retire(ctx context.Context, runID string, retain time.Duration) error {
	s.log.add("retire")
	return s.Store.Retire(ctx, runID, retain)
}

// recordingRedis wraps handlerFakeRedis, recording every SUCCESSFUL dual
// write as a "persist" marker (failed attempts are not persistence).
type recordingRedis struct {
	*handlerFakeRedis
	log *orderLog
}

func (r *recordingRedis) AppendMessagesDual(ctx context.Context, sessionID string, raws [][]byte) error {
	if err := r.handlerFakeRedis.AppendMessagesDual(ctx, sessionID, raws); err != nil {
		return err
	}
	r.log.add("persist")
	return nil
}

// persistTestEnv is a minimal SendMessage harness with recording seams on the
// run store, the Redis dual write, and the drain primitive.
type persistTestEnv struct {
	engine *gin.Engine
	store  *recordingRunStore
	redis  *recordingRedis
	mysql  *handlerFakeMySQL
	log    *orderLog
	reg    *run.Registry
	// drainHold, when non-nil, blocks the turn-end drain after it is recorded
	// until the channel closes — a timing probe for shutdown-order tests.
	drainHold chan struct{}
}

func newPersistTestEnv(t *testing.T, stub *stubOrchestrator, drainErr error) *persistTestEnv {
	t.Helper()
	log := &orderLog{}
	env := &persistTestEnv{log: log}
	mysql := &handlerFakeMySQL{
		getSessionByIDFound: &model.Session{SessionID: "sess-1", UserID: "user-1"},
	}
	redis := &recordingRedis{handlerFakeRedis: &handlerFakeRedis{}, log: log}
	drain := func(ctx context.Context) error {
		log.add("drain")
		// Hold only the TURN-END drain (recognizable by an already-recorded
		// persist marker); the read-path miss-drain runs before any persist
		// and must pass through untouched.
		if env.drainHold != nil && logContains(log.snapshot(), "persist") {
			select {
			case <-env.drainHold:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return drainErr
	}
	deps := service.SessionDeps{
		MySQL:             mysql,
		Redis:             redis,
		FS:                newHandlerFakeFS(),
		DrainMessageQueue: drain,
	}
	sessSvc := service.NewSessionService(deps)
	msgSvc := service.NewMessageService(deps, sessSvc.SaveMessage)
	titleSvc := service.NewTitleService(nil, mysql, config.OpenAIConfig{TitleModel: "title-model"})
	store := &recordingRunStore{Store: run.NewMemStore(), log: log}
	reg := run.NewRegistry()
	streamH := NewMessageStreamHandler(sessSvc, msgSvc, titleSvc, nil, nil, stub,
		"/tmp/blowball-test-data", run.NewManager(store, reg), testSelectionConfig(), 0)

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-1")
		c.Next()
	})
	engine.POST("/api/v1/sessions/:session_id/messages", streamH.SendMessage)
	env.engine = engine
	env.store = store
	env.redis = redis
	env.mysql = mysql
	env.reg = reg
	return env
}

// postMessage drives one SendMessage request against env.
func postMessage(t *testing.T, env *persistTestEnv) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-1/messages",
		strings.NewReader(`{"content":"hi there"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	return w
}

// doneStub returns a stub emitting a minimal successful turn (with a usage
// object so the turn_usage path also executes).
func doneStub() *stubOrchestrator {
	return &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfucius),
			stream.TokenEvent(stream.AgentConfucius, "Hello"),
			stream.AgentEndEvent(stream.AgentConfucius),
			stream.DoneEvent(map[string]any{
				"total":    map[string]any{"prompt_tokens": 4, "completion_tokens": 6, "total_tokens": 10},
				"by_agent": map[string]any{"Confucius": map[string]any{"total_tokens": 10}},
				"meta":     map[string]any{"sub_agent_invocations": []string{}, "parallel": false},
			}),
		},
	}
}

// terminalOrder is the harden-turn-persistence terminal sequence for the
// turn's own persistence (the log may carry earlier markers from the read
// path's miss-drain and the send-time user-row write, which are correct and
// unrelated).
var terminalOrder = []string{"persist", "drain", "status", "release", "retire"}

// requireTerminalSuffix asserts seq ENDS WITH want.
func requireTerminalSuffix(t *testing.T, seq, want []string) {
	t.Helper()
	require.GreaterOrEqual(t, len(seq), len(want), "sequence %v shorter than suffix %v", seq, want)
	require.Equal(t, want, seq[len(seq)-len(want):], "full sequence: %v", seq)
}

// TestSendMessage_TerminalPersistBeforeClaimRelease_DonePath verifies the
// core harden-turn-persistence invariant on the success path: the terminal
// message batch is durable and the pre-release drain has run BEFORE Finalize
// flips the run terminal and releases the session claim — so generating:false
// implies a complete, MySQL-visible history.
func TestSendMessage_TerminalPersistBeforeClaimRelease_DonePath(t *testing.T) {
	env := newPersistTestEnv(t, doneStub(), nil)

	w := postMessage(t, env)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	requireTerminalSuffix(t, env.log.snapshot(), terminalOrder)
}

// TestSendMessage_TerminalPersistBeforeClaimRelease_ErrorPath repeats the
// ordering invariant on the orchestrator-error path: the partial event stream
// is persisted (and drained) before the run is marked error and unlocked.
// The cancel path shares the same turn-goroutine code sequence.
func TestSendMessage_TerminalPersistBeforeClaimRelease_ErrorPath(t *testing.T) {
	stub := doneStub()
	stub.returnErr = errors.New("upstream 429")
	env := newPersistTestEnv(t, stub, nil)

	w := postMessage(t, env)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	requireTerminalSuffix(t, env.log.snapshot(), terminalOrder)
}

// TestSendMessage_DrainFailure_DoesNotBlockFinalize verifies the bounded
// drain is best-effort: a drain failure logs a WARN but the claim is still
// released after the (successful) message batch — the background flusher
// closes the residual ≤1s visibility gap.
func TestSendMessage_DrainFailure_DoesNotBlockFinalize(t *testing.T) {
	env := newPersistTestEnv(t, doneStub(), errors.New("drain wedged"))

	w := postMessage(t, env)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	requireTerminalSuffix(t, env.log.snapshot(), terminalOrder)
}

// TestSendMessage_PersistBudgetExhausted_StillFinalizes verifies the bounded
// release: when every tier fails every attempt (send-time and all turn-end
// retries), the turn still finalizes and releases the claim instead of
// locking the session forever. The not-durable batch is now a loud, bounded
// ERROR — not a silent fire-and-forget drop.
func TestSendMessage_PersistBudgetExhausted_StillFinalizes(t *testing.T) {
	env := newPersistTestEnv(t, doneStub(), nil)
	env.redis.dualErr = errors.New("redis down")
	env.mysql.appendMessagesErr = errors.New("mysql down")

	w := postMessage(t, env)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Send-time: 1 attempt (deferral is the recovery). Turn-end: 3 bounded
	// retries. Every attempt fails at both tiers.
	require.Equal(t, 4, env.redis.dualCount())
	env.mysql.mu.Lock()
	calls := env.mysql.appendMessagesCalls
	env.mysql.mu.Unlock()
	require.Equal(t, 4, calls)

	// Nothing persisted, yet the terminal sequence still runs — the session
	// unlocks after the bounded budget.
	requireTerminalSuffix(t, env.log.snapshot(), []string{"drain", "status", "release", "retire"})
}

// TestSendMessage_SendTimeDualFailure_TurnEndBatchIncludesUserRow verifies
// the at-least-once fallback that the swallowed-error bug had disabled: when
// the send-time user-row write fails at BOTH tiers, the turn-end batch
// re-includes the user row, and both deliveries carry the SAME deterministic
// client_msg_id ({trace_id}:0) so MySQL's UNIQUE index collapses them to one
// row.
func TestSendMessage_SendTimeDualFailure_TurnEndBatchIncludesUserRow(t *testing.T) {
	env := newSessionHandlerEnv(t, doneStub())
	env.redis.dualErrFirst = 1
	env.mysql.appendMessagesErrFirst = 1

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-1/messages",
		strings.NewReader(`{"content":"hi there"}`))
	req.Header.Set("Content-Type", "application/json")
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	batches := env.redis.dualBatchSnapshot()
	require.Len(t, batches, 2, "send-time attempt (failed) + turn-end recovery (succeeded)")

	// The send-time delivery carried only the user row; the turn-end delivery
	// re-included it ahead of the assistant events.
	require.Len(t, batches[0], 1)
	require.Equal(t, model.RoleUser, batches[0][0].Role)
	require.Len(t, batches[1], 4, "user row + agent_start/token/agent_end rows")
	require.Equal(t, model.RoleUser, batches[1][0].Role)
	require.Equal(t, model.EventTypeAgentStart, batches[1][1].EventType)
	require.Equal(t, model.EventTypeToken, batches[1][2].EventType)
	require.Equal(t, model.EventTypeAgentEnd, batches[1][3].EventType)

	// Both deliveries of the user row carry the deterministic {trace_id}:0 —
	// the idempotency key that guarantees MySQL keeps exactly one row.
	require.Equal(t, "trace-1:0", batches[0][0].ClientMsgID)
	require.Equal(t, batches[0][0].ClientMsgID, batches[1][0].ClientMsgID)
}

// TestStartPersistHeartbeat verifies the persist-phase heartbeat keeper beats
// at the given cadence and stops cleanly (no goroutine leak, no beats after
// stop) — the D3 guarantee that a slow terminal persist cannot let the
// 15s alive TTL expire under an attach's dead-run check.
func TestStartPersistHeartbeat(t *testing.T) {
	store := &heartbeatRecordingStore{Store: run.NewMemStore()}
	stop := startPersistHeartbeat(context.Background(), store, "rid-1", "sess-1", 5*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if store.count() >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	after := store.count()
	require.GreaterOrEqual(t, after, 2, "keeper must beat repeatedly while armed")

	// After stop returns the keeper goroutine has exited: no further beats
	// regardless of how long we wait.
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, after, store.count(), "no heartbeats after stop")
}

// TestSendMessage_RegistryFinishWaitsForTerminalSequence verifies the
// graceful-shutdown guarantee (harden-turn-persistence D6): the run registry's
// Finish signal — what shutdown's WaitAll waits on — fires only AFTER the
// terminal persist + drain sequence completes. While the drain is held, the
// run is still registered (WaitAll would block); once released, WaitAll
// returns nil.
func TestSendMessage_RegistryFinishWaitsForTerminalSequence(t *testing.T) {
	env := newPersistTestEnv(t, doneStub(), nil)
	hold := make(chan struct{})
	env.drainHold = hold

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postMessage(t, env)
	}()

	// Wait until the turn reached the turn-end drain (persist is already
	// recorded before it) while the drain is held open.
	require.Eventually(t, func() bool {
		drains := 0
		for _, ev := range env.log.snapshot() {
			if ev == "drain" {
				drains++
			}
		}
		return drains >= 2 // read-miss drain + the held turn-end drain
	}, 5*time.Second, 10*time.Millisecond, "turn never reached the terminal drain")
	require.Contains(t, env.log.snapshot(), "persist", "terminal batch persisted before the drain probe")

	// The turn goroutine is parked inside the drain, strictly before Finish:
	// the run is still registered, so a bounded WaitAll must time out.
	if _, ok := env.reg.Get("trace-1"); !ok {
		t.Fatal("run finished before the terminal sequence completed")
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := env.reg.WaitAll(wctx)
	wcancel()
	require.ErrorIs(t, err, context.DeadlineExceeded, "WaitAll must not unblock before persist+drain complete")

	// Release the probe: the terminal sequence completes, Finish fires, and
	// WaitAll returns cleanly — the shutdown path needs no sleep heuristic.
	close(hold)
	w := <-done
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	wctx2, wcancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel2()
	require.NoError(t, env.reg.WaitAll(wctx2))
}

// logContains reports whether the snapshot contains ev.
func logContains(seq []string, ev string) bool {
	for _, s := range seq {
		if s == ev {
			return true
		}
	}
	return false
}

// heartbeatRecordingStore counts Heartbeat calls over a real MemStore.
type heartbeatRecordingStore struct {
	run.Store
	mu sync.Mutex
	n  int
}

func (s *heartbeatRecordingStore) Heartbeat(ctx context.Context, runID, sessionID string) error {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return s.Store.Heartbeat(ctx, runID, sessionID)
}

func (s *heartbeatRecordingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// TestIncrementalPersistence_ClosedEntriesLandDuringTurn verifies the core
// incremental-message-persistence semantics end to end with a slow turn:
// (a) closed merged events (agent_start; then the fully-merged token run once
// agent_end closes it) land in SEPARATE mid-turn batches while the turn is
// still running; (b) the still-growing token run is NEVER persisted early;
// (c) the terminal batch is a pure suffix; (d) across all batches every row
// (user + every merged event) appears exactly once with contiguous ordinals.
func TestIncrementalPersistence_ClosedEntriesLandDuringTurn(t *testing.T) {
	oldEvery := incrementalPersistEvery
	incrementalPersistEvery = 20 * time.Millisecond
	t.Cleanup(func() { incrementalPersistEvery = oldEvery })

	stub := &stubOrchestrator{
		interEventDelay: 150 * time.Millisecond,
		eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfucius),
			stream.TokenEvent(stream.AgentConfucius, "he"),  // grows…
			stream.TokenEvent(stream.AgentConfucius, "llo"), // …merges with above
			stream.AgentEndEvent(stream.AgentConfucius),     // closes the token run
			stream.DoneEvent(map[string]any{
				"total":    map[string]any{"prompt_tokens": 4, "completion_tokens": 6, "total_tokens": 10},
				"by_agent": map[string]any{"Confucius": map[string]any{"total_tokens": 10}},
				"meta":     map[string]any{"sub_agent_invocations": []string{}, "parallel": false},
			}),
		},
	}
	env := newSessionHandlerEnv(t, stub)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-1/messages",
		strings.NewReader(`{"content":"slow turn"}`))
	req.Header.Set("Content-Type", "application/json")
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	batches := env.redis.dualBatchSnapshot()
	require.GreaterOrEqual(t, len(batches), 3, "send-time + ≥1 incremental + terminal batches; got %d", len(batches))

	// Batch 0 is the send-time user row.
	require.Len(t, batches[0], 1)
	require.Equal(t, "trace-1:0", batches[0][0].ClientMsgID)

	// The open token run is NEVER persisted partially: while only "he" has
	// arrived the run is the last (still-growing) merged entry and must stay
	// out of every batch. Only once agent_end closes it may it land — and
	// then as the FULL merged text.
	for _, b := range batches {
		for _, m := range b {
			if m.EventType == model.EventTypeToken {
				require.Equal(t, "hello", m.Content,
					"token run persisted before closing (partial content %q)", m.Content)
			}
		}
	}

	// The mid-turn batch that contains agent_start carries the merged token
	// text only together with (i.e. after) the agent_end boundary: find the
	// incremental batch holding ordinal 2 (the merged token run) and verify
	// its content is the FULL merged text.
	var tokenRow *model.Message
	for _, b := range batches[1:] {
		for i := range b {
			if b[i].EventType == model.EventTypeToken {
				tokenRow = &b[i]
			}
		}
	}
	require.NotNil(t, tokenRow, "merged token run must be persisted somewhere")
	require.Equal(t, "hello", tokenRow.Content, "token fragments must arrive merged, never partial")

	// Global uniqueness: every client_msg_id exactly once across ALL batches.
	seen := map[string]int{}
	for _, b := range batches {
		for _, m := range b {
			seen[m.ClientMsgID]++
		}
	}
	for id, n := range seen {
		require.Equal(t, 1, n, "client_msg_id %s persisted %d times — ranges overlapped", id, n)
	}
	require.Equal(t, map[string]int{"trace-1:0": 1, "trace-1:1": 1, "trace-1:2": 1, "trace-1:3": 1}, seen,
		"user + agent_start + merged token + agent_end, contiguous ordinals, each exactly once")
}
