package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/rpcx"
)

type fakeStore struct {
	mu sync.Mutex

	claimable []model.ScheduledTaskRun
	claim     *model.ScheduledTaskRun
	claimOK   bool
	launchOK  bool

	recoverable  []model.ScheduledTaskRun
	held         []model.ScheduledTaskRun
	exhausted    []model.ScheduledTaskRun
	holdErr      error
	reconcileOps []string

	receiptGens  []uint64
	retries      []string
	requeues     []string
	finishes     []string
	interrupted  []string
	runningIDs   []string
	holds        []string
	clears       []string
	paused       []string
	exhaustedFin []string
}

func (f *fakeStore) MaterializeTasks(context.Context, time.Time, int) ([]model.ScheduledTaskRun, error) {
	return nil, nil
}

func (f *fakeStore) ListClaimableRuns(context.Context, time.Time, int, int) ([]model.ScheduledTaskRun, error) {
	return f.claimable, nil
}

func (f *fakeStore) ClaimRun(context.Context, string, time.Time, time.Duration, int) (*model.ScheduledTaskRun, bool, error) {
	return f.claim, f.claimOK, nil
}

func (f *fakeStore) MarkLaunching(context.Context, string, string, time.Time, time.Duration) (bool, error) {
	return f.launchOK, nil
}

func (f *fakeStore) SetReceiptGeneration(_ context.Context, _ string, generation uint64) error {
	f.mu.Lock()
	f.receiptGens = append(f.receiptGens, generation)
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) ScheduleRetry(_ context.Context, _, _, reason, _ string, _ time.Time) (bool, error) {
	f.mu.Lock()
	f.retries = append(f.retries, reason)
	f.mu.Unlock()
	return true, nil
}

func (f *fakeStore) RequeueRun(_ context.Context, _, reason, _ string, _ time.Time) (bool, error) {
	f.mu.Lock()
	f.requeues = append(f.requeues, reason)
	f.mu.Unlock()
	return true, nil
}

func (f *fakeStore) FinishBeforeStart(_ context.Context, _, _, status, reason, _ string, _ time.Time) (bool, error) {
	f.mu.Lock()
	f.finishes = append(f.finishes, status+":"+reason)
	f.mu.Unlock()
	return true, nil
}

func (f *fakeStore) FinishExhausted(_ context.Context, occurrenceID, status, reason, _ string, _ time.Time) (bool, error) {
	f.mu.Lock()
	f.exhaustedFin = append(f.exhaustedFin, status+":"+reason)
	f.mu.Unlock()
	return true, nil
}

func (f *fakeStore) FinishOccurrence(_ context.Context, _, status, reason, _ string, _ uint64, _ time.Time) (bool, error) {
	f.mu.Lock()
	f.finishes = append(f.finishes, status+":"+reason)
	f.mu.Unlock()
	return true, nil
}

func (f *fakeStore) MarkRunningFromReceipt(_ context.Context, occurrenceID string, _ uint64, _ *time.Time) error {
	f.mu.Lock()
	f.runningIDs = append(f.runningIDs, occurrenceID)
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) MarkInterruptedFromReceipt(_ context.Context, occurrenceID string, _ uint64, _ string, _ time.Time) (bool, error) {
	f.mu.Lock()
	f.reconcileOps = append(f.reconcileOps, "interrupted")
	f.interrupted = append(f.interrupted, occurrenceID)
	f.mu.Unlock()
	return true, nil
}

func (f *fakeStore) ListRecoverableRuns(context.Context, int) ([]model.ScheduledTaskRun, error) {
	return f.recoverable, nil
}

func (f *fakeStore) ListExhaustedRuns(context.Context, int, int) ([]model.ScheduledTaskRun, error) {
	return f.exhausted, nil
}

func (f *fakeStore) SetHold(_ context.Context, _, occurrenceID, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconcileOps = append(f.reconcileOps, "hold")
	if f.holdErr != nil {
		return f.holdErr
	}
	f.holds = append(f.holds, occurrenceID)
	return nil
}

func (f *fakeStore) ClearHold(_ context.Context, _, occurrenceID string) (bool, error) {
	f.mu.Lock()
	f.clears = append(f.clears, occurrenceID)
	f.mu.Unlock()
	return true, nil
}

func (f *fakeStore) ListHeldOccurrences(context.Context, int) ([]model.ScheduledTaskRun, error) {
	return f.held, nil
}

func (f *fakeStore) GetTaskInternal(context.Context, string) (*model.ScheduledTask, error) {
	return &model.ScheduledTask{TaskID: "task-1", Enabled: true}, nil
}

func (f *fakeStore) PauseTask(_ context.Context, taskID, _ string, _ time.Time) error {
	f.mu.Lock()
	f.paused = append(f.paused, taskID)
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) AuthorizeStart(context.Context, string, string, string, int, uint64, string, time.Time, time.Duration) (*model.ScheduledStartGrant, error) {
	return &model.ScheduledStartGrant{OccurrenceID: "occ-1", Generation: 1}, nil
}

func (f *fakeStore) GetStartGrant(context.Context, string, uint64) (*model.ScheduledStartGrant, error) {
	return &model.ScheduledStartGrant{OccurrenceID: "occ-1", Generation: 1}, nil
}

type fakeRuntime struct {
	submitResult *SubmitResult
	submitErr    error
	fact         *ExecutionFact
	factErr      error
	submits      []SubmitInput
}

func (f *fakeRuntime) SubmitScheduledRun(_ context.Context, in SubmitInput) (*SubmitResult, error) {
	f.submits = append(f.submits, in)
	return f.submitResult, f.submitErr
}

func (f *fakeRuntime) GetScheduledExecution(context.Context, string) (*ExecutionFact, error) {
	return f.fact, f.factErr
}

func runFixture(t *testing.T) (*Scheduler, *fakeStore, *fakeRuntime, *model.ScheduledTaskRun) {
	t.Helper()
	st := &fakeStore{claimOK: true, launchOK: true}
	rt := &fakeRuntime{}
	run := &model.ScheduledTaskRun{
		OccurrenceID: "occ-1", TaskID: "task-1", UserID: "user-1", RunID: "run-1",
		SessionID: "sess-1", Revision: 1, SnapshotJSON: `{"prompt":"p"}`,
		Status: model.ScheduledRunPreparing, ClaimToken: "claim-1", AttemptCount: 1,
		ScheduledFor: time.Now().UTC(),
	}
	st.claim = run
	s := New(st, rt, Config{PollInterval: time.Hour, ExecutionEnabled: true}, func() time.Time {
		return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	})
	return s, st, rt, run
}

func TestExecuteSubmitsStableIdentity(t *testing.T) {
	s, st, rt, run := runFixture(t)
	rt.submitResult = &SubmitResult{ReceiptGeneration: 4, Status: "accepted"}
	s.execute(context.Background(), run)

	require.Len(t, rt.submits, 1)
	in := rt.submits[0]
	assert.Equal(t, "occ-1", in.OccurrenceID)
	assert.Equal(t, "run-1", in.RunID)
	assert.Equal(t, "sess-1", in.SessionID)
	assert.Equal(t, 1, in.Attempt)
	assert.Len(t, in.ConfigFingerprint, 64)
	assert.Equal(t, []uint64{4}, st.receiptGens)
}

func TestExecuteAmbiguousSubmitUsesAuthoritativeReceipt(t *testing.T) {
	s, st, rt, run := runFixture(t)
	rt.submitErr = rpcx.Errorf(rpcx.CodeUnavailable, "response lost")
	rt.fact = &ExecutionFact{Status: "accepted", ReceiptGeneration: 2}

	s.execute(context.Background(), run)
	assert.Equal(t, []uint64{2}, st.receiptGens)
	assert.Empty(t, st.retries)
}

func TestExecuteAmbiguousSubmitWithoutReceiptRequeues(t *testing.T) {
	s, st, rt, run := runFixture(t)
	rt.submitErr = rpcx.Errorf(rpcx.CodeUnavailable, "scheduler offline")
	rt.fact = nil

	s.execute(context.Background(), run)
	assert.Equal(t, []string{model.ScheduledReasonAdmission}, st.retries)
}

func TestExecuteDefinitiveRejectionTerminalizes(t *testing.T) {
	s, st, rt, run := runFixture(t)
	rt.submitErr = rpcx.Errorf(rpcx.CodeFailedPrecondition, "task paused")
	rt.fact = nil

	s.execute(context.Background(), run)
	require.Len(t, st.finishes, 1)
	assert.Equal(t, model.ScheduledRunError+":"+model.ScheduledReasonStartRejected, st.finishes[0])
	assert.Empty(t, st.retries)
}

func TestReconcileProjectsTerminalAndClearsHold(t *testing.T) {
	s, st, rt, run := runFixture(t)
	run.GrantGeneration = 3
	run.Status = model.ScheduledRunRunning
	rt.fact = &ExecutionFact{
		Status: model.ScheduledReceiptSucceeded, Terminal: true, ReceiptGeneration: 3,
		StopProofPresent: true,
	}
	st.recoverable = []model.ScheduledTaskRun{*run}
	st.held = []model.ScheduledTaskRun{*run}

	require.NoError(t, s.reconcile(context.Background()))
	assert.Equal(t, []string{model.ScheduledRunSucceeded + ":"}, st.finishes)
	assert.Contains(t, st.clears, "occ-1")
}

func TestReconcileUncertainSetsHoldAndNeverReplays(t *testing.T) {
	s, st, rt, run := runFixture(t)
	run.GrantGeneration = 3
	run.Status = model.ScheduledRunRunning
	rt.fact = &ExecutionFact{Status: model.ScheduledReceiptUncertain, Uncertain: true, ReceiptGeneration: 3}
	st.recoverable = []model.ScheduledTaskRun{*run}

	require.NoError(t, s.reconcile(context.Background()))
	assert.Equal(t, []string{"occ-1"}, st.interrupted)
	assert.Equal(t, []string{"occ-1"}, st.holds)
	assert.Empty(t, st.retries)
	assert.Empty(t, rt.submits)
}

func TestReconcileUncertainRecordsHoldBeforeProjection(t *testing.T) {
	s, st, rt, run := runFixture(t)
	run.GrantGeneration = 3
	run.Status = model.ScheduledRunRunning
	rt.fact = &ExecutionFact{Status: model.ScheduledReceiptUncertain, Uncertain: true, ReceiptGeneration: 3}
	st.recoverable = []model.ScheduledTaskRun{*run}

	require.NoError(t, s.reconcile(t.Context()))
	assert.Equal(t, []string{"hold", "interrupted"}, st.reconcileOps,
		"the active occurrence cannot be released before its task hold is durable")
}

func TestReconcileUncertainHoldFailurePreservesRetry(t *testing.T) {
	s, st, rt, run := runFixture(t)
	run.GrantGeneration = 3
	run.Status = model.ScheduledRunRunning
	rt.fact = &ExecutionFact{Status: model.ScheduledReceiptUncertain, Uncertain: true, ReceiptGeneration: 3}
	st.recoverable = []model.ScheduledTaskRun{*run}
	st.holdErr = errors.New("hold persistence unavailable")

	require.NoError(t, s.reconcile(t.Context()))
	assert.Empty(t, st.interrupted, "a failed hold write must keep the occurrence active and recoverable")
	assert.Empty(t, st.holds)
	assert.Empty(t, rt.submits)

	st.holdErr = nil
	require.NoError(t, s.reconcile(t.Context()))
	assert.Equal(t, []string{"occ-1"}, st.holds)
	assert.Equal(t, []string{"occ-1"}, st.interrupted)
	assert.Equal(t, []string{"hold", "hold", "interrupted"}, st.reconcileOps)
	assert.Empty(t, rt.submits)
}

func TestReconcileRunningProjectsStart(t *testing.T) {
	s, st, rt, run := runFixture(t)
	run.GrantGeneration = 3
	run.Status = model.ScheduledRunLaunching
	started := time.Now().UTC()
	rt.fact = &ExecutionFact{Status: model.ScheduledReceiptRunning, Started: true, ReceiptGeneration: 3, StartedAt: &started}
	st.recoverable = []model.ScheduledTaskRun{*run}

	require.NoError(t, s.reconcile(context.Background()))
	assert.Equal(t, []string{"occ-1"}, st.runningIDs)
	assert.Empty(t, st.holds)
}

func TestReconcileMissingReceiptAfterGrantGoesUnknown(t *testing.T) {
	s, st, rt, run := runFixture(t)
	run.GrantGeneration = 3
	run.Status = model.ScheduledRunRunning
	rt.fact = nil
	st.recoverable = []model.ScheduledTaskRun{*run}

	require.NoError(t, s.reconcile(context.Background()))
	assert.Equal(t, []string{"occ-1"}, st.interrupted)
	assert.Equal(t, []string{"occ-1"}, st.holds)
}

func TestExhaustRetriesTerminalizes(t *testing.T) {
	s, st, _, run := runFixture(t)
	st.exhausted = []model.ScheduledTaskRun{*run}
	require.NoError(t, s.exhaustRetries(context.Background()))
	require.Len(t, st.exhaustedFin, 1)
	assert.Equal(t, model.ScheduledRunError+":"+model.ScheduledReasonStartRejected, st.exhaustedFin[0])
}

func TestExecutionDisabledSkipsCycle(t *testing.T) {
	st := &fakeStore{}
	rt := &fakeRuntime{}
	s := New(st, rt, Config{ExecutionEnabled: false})
	require.NoError(t, s.RunOnce(context.Background()))
	assert.Empty(t, rt.submits)
}

func TestAmbiguityClassification(t *testing.T) {
	assert.True(t, ambiguous(errors.New("plain transport")))
	assert.True(t, ambiguous(rpcx.Errorf(rpcx.CodeUnavailable, "down")))
	assert.True(t, ambiguous(rpcx.Errorf(rpcx.CodeDeadlineExceeded, "slow")))
	assert.False(t, ambiguous(rpcx.Errorf(rpcx.CodeStartRejected, "paused")))
	assert.False(t, ambiguous(rpcx.Errorf(rpcx.CodeInvalidArgument, "bad")))
}
