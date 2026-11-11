package mysql

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

func acceptFixture(t *testing.T) (*Store, model.ScheduledExecutionReceipt) {
	t.Helper()
	store, _, cleanup := setupScheduledStore(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	receipt := model.ScheduledExecutionReceipt{
		OccurrenceID: "occ-r1", TaskID: "task-1", UserID: "user-1",
		RunID: "run-r1", SessionID: "sess-r1", AttemptCount: 1,
		ConfigFingerprint: "fp-1", ReceiptGeneration: 1,
		Status: model.ScheduledReceiptAccepted, AcceptedAt: time.Now().UTC(),
	}
	session := model.Session{
		SessionID: "sess-r1", UserID: "user-1", TaskID: "task-1",
		OccurrenceID: "occ-r1", RunID: "run-r1", TraceID: "run-r1",
	}
	stored, already, err := store.AcceptScheduledExecution(ctx, receipt, session)
	require.NoError(t, err)
	require.False(t, already)
	require.Equal(t, model.ScheduledReceiptAccepted, stored.Status)
	return store, *stored
}

func TestAcceptScheduledExecutionIsIdempotent(t *testing.T) {
	store, stored := acceptFixture(t)
	ctx := context.Background()

	session := model.Session{
		SessionID: "sess-r1", UserID: "user-1", TaskID: "task-1",
		OccurrenceID: "occ-r1", RunID: "run-r1", TraceID: "run-r1",
	}
	again, already, err := store.AcceptScheduledExecution(ctx, stored, session)
	require.NoError(t, err)
	assert.True(t, already)
	assert.Equal(t, stored.ReceiptGeneration, again.ReceiptGeneration)

	conflict := stored
	conflict.ConfigFingerprint = "different"
	_, _, err = store.AcceptScheduledExecution(ctx, conflict, session)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrScheduledReceiptConflict)

	var count int
	require.NoError(t, store.db.GetContext(ctx, &count, "SELECT COUNT(*) FROM scheduled_execution_receipts"))
	assert.Equal(t, 1, count)
}

func TestAcceptScheduledExecutionConcurrentCallsAgree(t *testing.T) {
	store, stored := acceptFixture(t)
	ctx := context.Background()
	session := model.Session{
		SessionID: "sess-r1", UserID: "user-1", TaskID: "task-1",
		OccurrenceID: "occ-r1", RunID: "run-r1", TraceID: "run-r1",
	}

	var wg sync.WaitGroup
	results := make(chan bool, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, already, err := store.AcceptScheduledExecution(ctx, stored, session)
			if err != nil {
				errs <- err
				return
			}
			results <- already
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Len(t, results, 8)
	var count int
	require.NoError(t, store.db.GetContext(ctx, &count, "SELECT COUNT(*) FROM scheduled_execution_receipts"))
	assert.Equal(t, 1, count)
}

func TestReceiptFenceFlow(t *testing.T) {
	store, stored := acceptFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	claimed, ok, err := store.ClaimScheduledReceipt(ctx, stored.OccurrenceID, "worker-A", 30*time.Second, 0, now)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, model.ScheduledReceiptPreparing, claimed.Status)
	assert.Equal(t, uint64(1), claimed.ReceiptGeneration)

	ok, err = store.MarkScheduledReceiptStartIntent(ctx, stored.OccurrenceID, claimed.ReceiptGeneration, "worker-A", 30*time.Second, now)
	require.NoError(t, err)
	require.True(t, ok)

	grant := model.ScheduledStartGrant{
		OccurrenceID: stored.OccurrenceID, Generation: 7, RunID: stored.RunID, SessionID: stored.SessionID,
		StartedAt: now, Deadline: now.Add(10 * time.Minute),
	}
	ok, err = store.MarkScheduledReceiptRunning(ctx, stored.OccurrenceID, claimed.ReceiptGeneration, "worker-A", grant)
	require.NoError(t, err)
	require.True(t, ok)

	running, err := store.GetScheduledExecution(ctx, stored.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledReceiptRunning, running.Status)
	assert.Equal(t, uint64(7), running.GrantGeneration)
	assert.False(t, running.StopProofPresent)

	ok, err = store.FinishScheduledReceipt(ctx, stored.OccurrenceID, claimed.ReceiptGeneration, "worker-A",
		model.ScheduledReceiptSucceeded, "", "", now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, ok)

	final, err := store.GetScheduledExecution(ctx, stored.OccurrenceID)
	require.NoError(t, err)
	assert.True(t, final.Terminal())
	assert.True(t, final.StopProofPresent)
	assert.Equal(t, claimed.ReceiptGeneration, final.StopProofGen)

	// A second terminal write (late worker or duplicate) is a no-op.
	ok, err = store.FinishScheduledReceipt(ctx, stored.OccurrenceID, claimed.ReceiptGeneration, "worker-A",
		model.ScheduledReceiptError, "", "", now.Add(2*time.Minute))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestReclaimFencesTheOldPreparationWorker(t *testing.T) {
	store, stored := acceptFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first, ok, err := store.ClaimScheduledReceipt(ctx, stored.OccurrenceID, "worker-A", time.Second, 0, now)
	require.NoError(t, err)
	require.True(t, ok)

	// Simulate the first worker's lease expiring.
	_, err = store.db.ExecContext(ctx, "UPDATE scheduled_execution_receipts SET lease_until = ? WHERE occurrence_id = ?",
		now.Add(-time.Minute), stored.OccurrenceID)
	require.NoError(t, err)

	second, ok, err := store.ClaimScheduledReceipt(ctx, stored.OccurrenceID, "worker-B", 30*time.Second, 0, now)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, first.ReceiptGeneration+1, second.ReceiptGeneration, "reclaim fences the previous preparation generation")
	assert.Equal(t, "worker-B", second.WorkerGeneration)

	ok, err = store.MarkScheduledReceiptStartIntent(ctx, stored.OccurrenceID, first.ReceiptGeneration, "worker-A", 30*time.Second, now)
	require.NoError(t, err)
	assert.False(t, ok, "the stale worker cannot cross start_intent")

	ok, err = store.MarkScheduledReceiptStartIntent(ctx, stored.OccurrenceID, second.ReceiptGeneration, "worker-B", 30*time.Second, now)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestCancelBeforeStartWritesNotStartedProof(t *testing.T) {
	store, stored := acceptFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_, ok, err := store.ClaimScheduledReceipt(ctx, stored.OccurrenceID, "worker-A", 30*time.Second, 0, now)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = store.CancelScheduledReceiptBeforeStart(ctx, stored.OccurrenceID, 1, model.ScheduledReasonCancelledByUser, "", now)
	require.NoError(t, err)
	require.True(t, ok)

	rec, err := store.GetScheduledExecution(ctx, stored.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledReceiptCancelled, rec.Status)
	assert.True(t, rec.StopProofPresent)
	assert.Equal(t, model.ScheduledStopProofNotStarted, rec.StopProofKind)

	ok, err = store.MarkScheduledReceiptStartIntent(ctx, stored.OccurrenceID, 1, "worker-A", 30*time.Second, now)
	require.NoError(t, err)
	assert.False(t, ok, "a cancelled pre-start receipt cannot start")
}

func TestUncertainCanBeFinishedOrProvenStopped(t *testing.T) {
	store, stored := acceptFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	claimed, ok, err := store.ClaimScheduledReceipt(ctx, stored.OccurrenceID, "worker-A", 30*time.Second, 0, now)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = store.MarkScheduledReceiptStartIntent(ctx, stored.OccurrenceID, claimed.ReceiptGeneration, "worker-A", 30*time.Second, now)
	require.NoError(t, err)
	require.True(t, ok)

	// Reconciliation loses the worker.
	ok, err = store.MarkScheduledReceiptUncertain(ctx, stored.OccurrenceID, claimed.ReceiptGeneration,
		model.ScheduledReasonResultUnknown, "heartbeat absent", now)
	require.NoError(t, err)
	require.True(t, ok)

	// The authentic worker returns and reports a terminal fact: allowed, and it
	// carries a stop proof.
	ok, err = store.FinishScheduledReceipt(ctx, stored.OccurrenceID, claimed.ReceiptGeneration, "worker-A",
		model.ScheduledReceiptSucceeded, "", "", now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, ok)

	rec, err := store.GetScheduledExecution(ctx, stored.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledReceiptSucceeded, rec.Status)
	assert.True(t, rec.StopProofPresent)
}

func TestControlledStopProofForUncertain(t *testing.T) {
	store, stored := acceptFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	claimed, ok, err := store.ClaimScheduledReceipt(ctx, stored.OccurrenceID, "worker-A", 30*time.Second, 0, now)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = store.MarkScheduledReceiptStartIntent(ctx, stored.OccurrenceID, claimed.ReceiptGeneration, "worker-A", 30*time.Second, now)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = store.MarkScheduledReceiptUncertain(ctx, stored.OccurrenceID, claimed.ReceiptGeneration,
		model.ScheduledReasonResultUnknown, "worker lost", now)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = store.RecordScheduledStopProof(ctx, stored.OccurrenceID, claimed.ReceiptGeneration,
		model.ScheduledStopProofReconciled, now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	rec, err := store.GetScheduledExecution(ctx, stored.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledReceiptInterrupted, rec.Status)
	assert.Equal(t, model.ScheduledStopProofReconciled, rec.StopProofKind)
}

func TestCapacityAndLiveScan(t *testing.T) {
	store, stored := acceptFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	active, err := store.CountActiveScheduledReceipts(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, active)

	_, ok, err := store.ClaimScheduledReceipt(ctx, stored.OccurrenceID, "worker-A", 30*time.Second, 1, now)
	require.NoError(t, err)
	require.True(t, ok)
	active, err = store.CountActiveScheduledReceipts(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, active)

	// A second occurrence cannot be claimed while the first occupies the
	// configured global capacity.
	second := stored
	second.OccurrenceID = "occ-r2"
	second.RunID = "run-r2"
	second.SessionID = "sess-r2"
	secondSession := model.Session{SessionID: "sess-r2", UserID: "user-1", TaskID: "task-1", OccurrenceID: "occ-r2", RunID: "run-r2", TraceID: "run-r2"}
	_, _, err = store.AcceptScheduledExecution(ctx, second, secondSession)
	require.NoError(t, err)
	_, ok, err = store.ClaimScheduledReceipt(ctx, second.OccurrenceID, "worker-B", 30*time.Second, 1, now)
	require.NoError(t, err)
	assert.False(t, ok, "maxActive is enforced under the admission lock")

	live, err := store.ListLiveScheduledReceipts(ctx, 10)
	require.NoError(t, err)
	// preparing is not "live" (start_intent/running only).
	assert.Empty(t, live)
}
