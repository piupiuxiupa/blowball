package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/stream"
)

// queueTestEntry builds a queued entry + its meta + the queued event, as the
// enqueue path would leave them (session-message-queue): a running turn
// (trace-1) holds the claim, and the queued message waits behind it.
func seedQueuedRun(t *testing.T, env *turnRunTestEnv, runID string, newlyClaimed ...string) run.QueueEntry {
	t.Helper()
	ctx := context.Background()
	entry := run.QueueEntry{
		RunID:       runID,
		SessionID:   "sess-1",
		UserID:      "user-1",
		Content:     "queued " + runID,
		Model:       "gpt-test",
		NewlyClaimed: newlyClaimed,
		EnqueuedAt:  time.Unix(1700000000, 0).UTC(),
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
