package scheduledruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/modelselect"
	"github.com/lush/blowball/internal/rpcx"
	"github.com/lush/blowball/internal/turn"
)

// ── in-memory receipt store ────────────────────────────────────────────────

type fakeReceiptStore struct {
	mu       sync.Mutex
	receipts map[string]*model.ScheduledExecutionReceipt
	sessions map[string]model.Session
	failMark bool
}

func newFakeReceiptStore() *fakeReceiptStore {
	return &fakeReceiptStore{
		receipts: map[string]*model.ScheduledExecutionReceipt{},
		sessions: map[string]model.Session{},
	}
}

func cloneReceipt(r *model.ScheduledExecutionReceipt) *model.ScheduledExecutionReceipt {
	c := *r
	return &c
}

func (f *fakeReceiptStore) AcceptScheduledExecution(_ context.Context, receipt model.ScheduledExecutionReceipt, session model.Session) (*model.ScheduledExecutionReceipt, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.receipts[receipt.OccurrenceID]; ok {
		if existing.ConfigFingerprint != receipt.ConfigFingerprint || existing.TaskID != receipt.TaskID ||
			existing.RunID != receipt.RunID || existing.SessionID != receipt.SessionID {
			return nil, false, errors.New("receipt conflict")
		}
		if receipt.AttemptCount > existing.AttemptCount {
			existing.AttemptCount = receipt.AttemptCount
		}
		return cloneReceipt(existing), true, nil
	}
	c := receipt
	c.CreateTime = time.Now().UTC()
	c.UpdateTime = c.CreateTime
	f.receipts[receipt.OccurrenceID] = &c
	f.sessions[session.SessionID] = session
	return cloneReceipt(&c), false, nil
}

func (f *fakeReceiptStore) GetScheduledExecution(_ context.Context, occurrenceID string) (*model.ScheduledExecutionReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.receipts[occurrenceID]
	if !ok {
		return nil, nil
	}
	return cloneReceipt(rec), nil
}

func (f *fakeReceiptStore) GetScheduledExecutionBySession(_ context.Context, sessionID string) (*model.ScheduledExecutionReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rec := range f.receipts {
		if rec.SessionID == sessionID {
			return cloneReceipt(rec), nil
		}
	}
	return nil, nil
}

func (f *fakeReceiptStore) ListClaimableScheduledReceipts(_ context.Context, now time.Time, _ int) ([]model.ScheduledExecutionReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.ScheduledExecutionReceipt
	for _, rec := range f.receipts {
		if rec.Status == model.ScheduledReceiptAccepted ||
			(rec.Status == model.ScheduledReceiptPreparing && (rec.LeaseUntil == nil || rec.LeaseUntil.Before(now))) {
			out = append(out, *cloneReceipt(rec))
		}
	}
	return out, nil
}

func (f *fakeReceiptStore) ListLiveScheduledReceipts(_ context.Context, _ int) ([]model.ScheduledExecutionReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.ScheduledExecutionReceipt
	for _, rec := range f.receipts {
		if rec.Status == model.ScheduledReceiptStartIntent || rec.Status == model.ScheduledReceiptRunning {
			out = append(out, *cloneReceipt(rec))
		}
	}
	return out, nil
}

func (f *fakeReceiptStore) ClaimScheduledReceipt(_ context.Context, occurrenceID, worker string, lease time.Duration, _ int, now time.Time) (*model.ScheduledExecutionReceipt, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.receipts[occurrenceID]
	if !ok {
		return nil, false, nil
	}
	claimable := rec.Status == model.ScheduledReceiptAccepted ||
		(rec.Status == model.ScheduledReceiptPreparing && (rec.LeaseUntil == nil || rec.LeaseUntil.Before(now)))
	if !claimable {
		return nil, false, nil
	}
	if rec.Status == model.ScheduledReceiptPreparing {
		rec.ReceiptGeneration++
	}
	rec.Status = model.ScheduledReceiptPreparing
	rec.WorkerGeneration = worker
	until := now.Add(lease)
	rec.LeaseUntil = &until
	rec.Reason = ""
	return cloneReceipt(rec), true, nil
}

func (f *fakeReceiptStore) RenewScheduledReceiptLease(context.Context, string, string, time.Duration, time.Time) (bool, error) {
	return true, nil
}

func (f *fakeReceiptStore) ReleaseScheduledReceiptForRetry(_ context.Context, occurrenceID string, generation uint64, worker, reason, _ string, maxAttempts int, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.receipts[occurrenceID]
	if !ok || rec.ReceiptGeneration != generation || rec.WorkerGeneration != worker ||
		(rec.Status != model.ScheduledReceiptAccepted && rec.Status != model.ScheduledReceiptPreparing) {
		return false, nil
	}
	rec.RuntimeAttempts++
	if rec.RuntimeAttempts < maxAttempts {
		rec.Status = model.ScheduledReceiptAccepted
		rec.WorkerGeneration = ""
		rec.LeaseUntil = nil
		rec.Reason = reason
		return true, nil
	}
	rec.Status = model.ScheduledReceiptError
	rec.Reason = reason
	rec.StopProofPresent = true
	rec.StopProofKind = model.ScheduledStopProofNotStarted
	fin := now.UTC()
	rec.FinishedAt = &fin
	return false, nil
}

func (f *fakeReceiptStore) ReleaseScheduledReceiptOnShutdown(_ context.Context, occurrenceID string, generation uint64, worker string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.receipts[occurrenceID]
	if !ok || rec.ReceiptGeneration != generation || rec.WorkerGeneration != worker {
		return false, nil
	}
	rec.Status = model.ScheduledReceiptAccepted
	rec.WorkerGeneration = ""
	rec.LeaseUntil = nil
	return true, nil
}

func (f *fakeReceiptStore) MarkScheduledReceiptStartIntent(_ context.Context, occurrenceID string, generation uint64, worker string, lease time.Duration, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.receipts[occurrenceID]
	if !ok || rec.ReceiptGeneration != generation || rec.WorkerGeneration != worker || rec.Status != model.ScheduledReceiptPreparing {
		return false, nil
	}
	rec.Status = model.ScheduledReceiptStartIntent
	t := now.UTC()
	rec.StartIntentAt = &t
	until := now.Add(lease)
	rec.LeaseUntil = &until
	return true, nil
}

func (f *fakeReceiptStore) MarkScheduledReceiptRunning(_ context.Context, occurrenceID string, generation uint64, worker string, grant model.ScheduledStartGrant) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.receipts[occurrenceID]
	if !ok || rec.ReceiptGeneration != generation || rec.WorkerGeneration != worker || rec.Status != model.ScheduledReceiptStartIntent {
		return false, nil
	}
	rec.Status = model.ScheduledReceiptRunning
	rec.GrantGeneration = grant.Generation
	t := grant.StartedAt.UTC()
	rec.StartedAt = &t
	d := grant.Deadline.UTC()
	rec.GrantDeadline = &d
	rec.LeaseUntil = nil
	return true, nil
}

func (f *fakeReceiptStore) FinishScheduledReceipt(_ context.Context, occurrenceID string, generation uint64, worker, status, reason, _ string, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.receipts[occurrenceID]
	if !ok || rec.ReceiptGeneration != generation || rec.WorkerGeneration != worker || rec.StopProofPresent {
		return false, nil
	}
	switch rec.Status {
	case model.ScheduledReceiptPreparing, model.ScheduledReceiptStartIntent, model.ScheduledReceiptRunning, model.ScheduledReceiptUncertain:
	default:
		return false, nil
	}
	rec.Status = status
	rec.Reason = reason
	rec.StopProofPresent = true
	rec.StopProofGen = generation
	rec.StopProofKind = model.ScheduledStopProofTerminal
	t := now.UTC()
	rec.FinishedAt = &t
	rec.StopProofAt = &t
	rec.LeaseUntil = nil
	return true, nil
}

func (f *fakeReceiptStore) FinishScheduledReceiptBeforeStart(_ context.Context, occurrenceID string, generation uint64, worker, status, reason, _ string, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.receipts[occurrenceID]
	if !ok || rec.ReceiptGeneration != generation || rec.WorkerGeneration != worker {
		return false, nil
	}
	if rec.Status != model.ScheduledReceiptAccepted && rec.Status != model.ScheduledReceiptPreparing {
		return false, nil
	}
	rec.Status = status
	rec.Reason = reason
	rec.StopProofPresent = true
	rec.StopProofGen = generation
	rec.StopProofKind = model.ScheduledStopProofNotStarted
	t := now.UTC()
	rec.FinishedAt = &t
	rec.StopProofAt = &t
	rec.LeaseUntil = nil
	return true, nil
}

func (f *fakeReceiptStore) FinishScheduledReceiptStartRejected(_ context.Context, occurrenceID string, generation uint64, worker, status, reason, _ string, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.receipts[occurrenceID]
	if !ok || rec.ReceiptGeneration != generation || rec.WorkerGeneration != worker || rec.Status != model.ScheduledReceiptStartIntent {
		return false, nil
	}
	rec.Status = status
	rec.Reason = reason
	rec.StopProofPresent = true
	rec.StopProofGen = generation
	rec.StopProofKind = model.ScheduledStopProofNotStarted
	t := now.UTC()
	rec.FinishedAt = &t
	rec.StopProofAt = &t
	rec.LeaseUntil = nil
	return true, nil
}

func (f *fakeReceiptStore) MarkScheduledReceiptUncertain(_ context.Context, occurrenceID string, generation uint64, reason, _ string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.receipts[occurrenceID]
	if !ok || rec.ReceiptGeneration != generation || rec.StopProofPresent {
		return false, nil
	}
	if rec.Status != model.ScheduledReceiptStartIntent && rec.Status != model.ScheduledReceiptRunning {
		return false, nil
	}
	rec.Status = model.ScheduledReceiptUncertain
	rec.Reason = reason
	rec.LeaseUntil = nil
	return true, nil
}

func (f *fakeReceiptStore) RecordScheduledStopProof(_ context.Context, occurrenceID string, generation uint64, kind string, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.receipts[occurrenceID]
	if !ok || rec.ReceiptGeneration != generation || rec.Status != model.ScheduledReceiptUncertain || rec.StopProofPresent {
		return false, nil
	}
	rec.Status = model.ScheduledReceiptInterrupted
	rec.StopProofPresent = true
	rec.StopProofGen = generation
	rec.StopProofKind = kind
	t := now.UTC()
	rec.FinishedAt = &t
	rec.StopProofAt = &t
	return true, nil
}

func (f *fakeReceiptStore) CancelScheduledReceiptBeforeStart(_ context.Context, occurrenceID string, generation uint64, reason, _ string, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.receipts[occurrenceID]
	if !ok || rec.ReceiptGeneration != generation ||
		(rec.Status != model.ScheduledReceiptAccepted && rec.Status != model.ScheduledReceiptPreparing) {
		return false, nil
	}
	rec.Status = model.ScheduledReceiptCancelled
	rec.Reason = reason
	rec.StopProofPresent = true
	rec.StopProofGen = generation
	rec.StopProofKind = model.ScheduledStopProofNotStarted
	t := now.UTC()
	rec.FinishedAt = &t
	rec.StopProofAt = &t
	rec.LeaseUntil = nil
	return true, nil
}

func (f *fakeReceiptStore) CountActiveScheduledReceipts(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, rec := range f.receipts {
		if rec.Active() {
			n++
		}
	}
	return n, nil
}

// ── other fakes ────────────────────────────────────────────────────────────

type fakeOccurrences struct {
	occ        *Occurrence
	getErr     error
	authorize  func(AuthorizeStartRequest) (*Grant, error)
	getGrant   func(string, uint64) (*Grant, error)
	authorized []AuthorizeStartRequest
}

func (f *fakeOccurrences) GetOccurrence(context.Context, string) (*Occurrence, error) {
	return f.occ, f.getErr
}

func (f *fakeOccurrences) AuthorizeStart(_ context.Context, req AuthorizeStartRequest) (*Grant, error) {
	f.authorized = append(f.authorized, req)
	if f.authorize == nil {
		return &Grant{
			OccurrenceID: req.OccurrenceID, Generation: req.ReceiptGeneration,
			RunID: req.RunID, SessionID: req.SessionID,
			StartedAt: time.Now().UTC(), Deadline: time.Now().Add(time.Hour).UTC(),
		}, nil
	}
	return f.authorize(req)
}

func (f *fakeOccurrences) GetStartGrant(_ context.Context, occurrenceID string, generation uint64) (*Grant, error) {
	if f.getGrant != nil {
		return f.getGrant(occurrenceID, generation)
	}
	return nil, rpcx.Errorf(rpcx.CodeNotFound, "no grant")
}

type fakeUsers struct{ user *model.User }

func (f *fakeUsers) GetUserByID(context.Context, string) (*model.User, error) { return f.user, nil }

type fakeCanceller struct {
	mu       sync.Mutex
	runs     []string
	onCancel func(runID string)
}

func (f *fakeCanceller) RequestCancel(_ context.Context, runID string) error {
	f.mu.Lock()
	f.runs = append(f.runs, runID)
	cb := f.onCancel
	f.mu.Unlock()
	if cb != nil {
		cb(runID)
	}
	return nil
}

type fakeObserver struct {
	alive     bool
	inProcess bool
}

func (f *fakeObserver) Alive(context.Context, string) (bool, error) { return f.alive, nil }
func (f *fakeObserver) InProcess(string) bool                       { return f.inProcess }

type fakeTokens struct{ err error }

func (f *fakeTokens) Issue(context.Context, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "exec-token", nil
}

type fakeResources struct {
	contextText string
	err         error
}

func (f *fakeResources) ValidatePinned(context.Context, string, string, uint64, string) (string, error) {
	return f.contextText, f.err
}

type fakeTurns struct {
	mu            sync.Mutex
	requests      []*turn.Request
	beforeErr     error
	terminal      turn.TerminalResult
	callTerminal  bool
	terminalCalls int
	beforeCall    func(*turn.Request)
	forceOutcome  *turn.Submission
}

func (f *fakeTurns) Submit(ctx context.Context, req *turn.Request) turn.Submission {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	forced := f.forceOutcome
	f.mu.Unlock()
	if forced != nil {
		return *forced
	}
	if req.BeforeStart != nil {
		if err := req.BeforeStart(ctx); err != nil {
			return turn.Submission{Outcome: turn.SubmitRejected, Err: err}
		}
	}
	if f.beforeCall != nil {
		f.beforeCall(req)
	}
	if f.callTerminal && req.OnTerminal != nil {
		f.mu.Lock()
		f.terminalCalls++
		f.mu.Unlock()
		req.OnTerminal(ctx, f.terminal)
	}
	ch := make(chan turn.Result, 1)
	ch <- turn.Result{Err: f.terminal.Err}
	return turn.Submission{Started: &turn.Started{ResultCh: ch}, Outcome: turn.SubmitStarted}
}

func (f *fakeTurns) lastRequest() *turn.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

// ── fixture ────────────────────────────────────────────────────────────────

func runtimeFixture(t *testing.T) (*Runtime, *fakeReceiptStore, *fakeOccurrences, *fakeTurns, *fakeCanceller) {
	t.Helper()
	store := newFakeReceiptStore()
	snapshot := model.ScheduledTaskSnapshot{
		TaskID: "task-1", UserID: "user-1", Revision: 2, Name: "nightly",
		Prompt: "summarize the workspace",
		Model:  "model-a", ReasoningEffort: "medium",
	}
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	occ := &Occurrence{
		OccurrenceID: "occ-1", TaskID: "task-1", UserID: "user-1", TaskRevision: 2,
		SnapshotJSON: string(raw), RunID: "run-1", SessionID: "sess-1",
		SessionSource: "scheduled", Status: "launching", AttemptCount: 1,
	}
	occSource := &fakeOccurrences{occ: occ}
	turns := &fakeTurns{callTerminal: true, terminal: turn.TerminalResult{
		RunID: "run-1", SessionID: "sess-1", UserID: "user-1",
		Status: "done", MessagesDurable: true,
	}}
	canceller := &fakeCanceller{}
	rt, err := New(Dependencies{
		Store: store, Occurrences: occSource,
		Users:     &fakeUsers{user: &model.User{UserID: "user-1", Status: model.UserStatusActive}},
		Turns:     turns,
		Canceller: canceller,
		Observer:  &fakeObserver{alive: true},
		Tokens:    &fakeTokens{},
		Selection: modelselect.Config{
			Catalog: []config.ModelCatalogEntry{{Name: "model-a", MaxContextTokens: 100000, MaxCompletionTokens: 8000, Thinking: true}},
			Default: "model-a", DefaultEffort: "none",
		},
		Config:   Config{PollInterval: time.Hour},
		WorkerID: "worker-test",
	})
	require.NoError(t, err)
	return rt, store, occSource, turns, canceller
}

func seedAccepted(t *testing.T, rt *Runtime) *model.ScheduledExecutionReceipt {
	t.Helper()
	occ := rt.occurrences.(*fakeOccurrences).occ
	fp := fingerprintOf(occ)
	res, err := rt.Submit(context.Background(), SubmitInput{
		OccurrenceID: occ.OccurrenceID, TaskID: occ.TaskID, RunID: occ.RunID,
		SessionID: occ.SessionID, Attempt: occ.AttemptCount, ConfigFingerprint: fp,
	})
	require.NoError(t, err)
	return res.Receipt
}

func TestSubmitIsIdempotentAndRejectsFingerprintChange(t *testing.T) {
	rt, store, _, _, _ := runtimeFixture(t)
	ctx := context.Background()
	first := seedAccepted(t, rt)

	occ := rt.occurrences.(*fakeOccurrences).occ
	second, err := rt.Submit(ctx, SubmitInput{
		OccurrenceID: occ.OccurrenceID, TaskID: occ.TaskID, RunID: occ.RunID,
		SessionID: occ.SessionID, Attempt: 2, ConfigFingerprint: fingerprintOf(occ),
	})
	require.NoError(t, err)
	assert.True(t, second.AlreadyAccepted)
	assert.Equal(t, first.ReceiptGeneration, second.Receipt.ReceiptGeneration)
	assert.Equal(t, 2, second.Receipt.AttemptCount, "retry control may advance")

	_, err = rt.Submit(ctx, SubmitInput{
		OccurrenceID: occ.OccurrenceID, TaskID: occ.TaskID, RunID: occ.RunID,
		SessionID: occ.SessionID, ConfigFingerprint: "different",
	})
	require.Error(t, err)
	assert.Equal(t, rpcx.CodeConflict, mustCode(t, err))

	// A scheduled occurrence is never accepted when its authoritative source is
	// legacy interactive.
	occ.SessionSource = "legacy_interactive"
	_, err = rt.Submit(ctx, SubmitInput{
		OccurrenceID: "occ-legacy", TaskID: occ.TaskID, RunID: occ.RunID,
		SessionID: occ.SessionID, ConfigFingerprint: fingerprintOf(occ),
	})
	require.Error(t, err)
	assert.Equal(t, rpcx.CodeSessionPolicy, mustCode(t, err))
	occ.SessionSource = "scheduled"
	_ = store
}

func TestSubmitRejectsUnavailableOwnerAndUnknownModel(t *testing.T) {
	rt, _, _, _, _ := runtimeFixture(t)
	ctx := context.Background()
	occ := rt.occurrences.(*fakeOccurrences).occ

	rt.users = &fakeUsers{user: &model.User{UserID: "user-1", Status: model.UserStatusDisabled}}
	_, err := rt.Submit(ctx, SubmitInput{
		OccurrenceID: occ.OccurrenceID, TaskID: occ.TaskID, RunID: occ.RunID,
		SessionID: occ.SessionID, ConfigFingerprint: fingerprintOf(occ),
	})
	require.Error(t, err)
	assert.Equal(t, rpcx.CodeUserUnavailable, mustCode(t, err))

	rt.users = &fakeUsers{user: &model.User{UserID: "user-1", Status: model.UserStatusActive}}
	var snap model.ScheduledTaskSnapshot
	require.NoError(t, json.Unmarshal([]byte(occ.SnapshotJSON), &snap))
	snap.Model = "not-in-catalog"
	raw, _ := json.Marshal(snap)
	occ.SnapshotJSON = string(raw)
	_, err = rt.Submit(ctx, SubmitInput{
		OccurrenceID: occ.OccurrenceID, TaskID: occ.TaskID, RunID: occ.RunID,
		SessionID: occ.SessionID, ConfigFingerprint: fingerprintOf(occ),
	})
	require.Error(t, err)
	assert.Equal(t, rpcx.CodeInvalidModel, mustCode(t, err))
}

func TestExecuteRunsScheduledTurnAndWritesTerminalFact(t *testing.T) {
	rt, store, occSource, turns, _ := runtimeFixture(t)
	ctx := context.Background()
	rec := seedAccepted(t, rt)

	rt.execute(ctx, *rec)

	req := turns.lastRequest()
	require.NotNil(t, req)
	assert.Equal(t, model.SessionSourceScheduled, req.Source)
	assert.Equal(t, "summarize the workspace", req.Content)
	assert.Equal(t, "model-a", req.Selection.Model)
	assert.Equal(t, "medium", req.Selection.Effort)
	assert.Equal(t, "exec-token", req.RawToken)

	got, err := store.GetScheduledExecution(ctx, rec.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledReceiptSucceeded, got.Status)
	assert.True(t, got.StopProofPresent)
	assert.Equal(t, model.ScheduledStopProofTerminal, got.StopProofKind)
	assert.Len(t, occSource.authorized, 1)
	assert.Equal(t, rec.ReceiptGeneration, occSource.authorized[0].ReceiptGeneration)
	assert.NotContains(t, receiptDebugString(got), "exec-token",
		"the in-memory execution credential must never reach the receipt")
}

func receiptDebugString(rec *model.ScheduledExecutionReceipt) string {
	raw, _ := json.Marshal(rec)
	return string(raw)
}

func TestExecuteAdmissionFailureIsBoundedRetry(t *testing.T) {
	rt, store, _, turns, _ := runtimeFixture(t)
	ctx := context.Background()
	rec := seedAccepted(t, rt)
	turns.forceOutcome = &turn.Submission{Outcome: turn.SubmitAdmissionFailed, Err: errors.New("redis down")}
	rt.cfg.MaxPrepareAttempts = 2

	rt.execute(ctx, *rec)
	first, err := store.GetScheduledExecution(ctx, rec.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledReceiptAccepted, first.Status, "a pre-start admission failure is retried")

	rt.execute(ctx, *first)
	second, err := store.GetScheduledExecution(ctx, rec.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledReceiptError, second.Status)
	assert.Equal(t, model.ScheduledReasonAdmission, second.Reason)
	assert.Equal(t, model.ScheduledStopProofNotStarted, second.StopProofKind)
}

func TestExecuteUsesPinnedResourceContext(t *testing.T) {
	rt, _, _, turns, _ := runtimeFixture(t)
	ctx := context.Background()
	occ := rt.occurrences.(*fakeOccurrences).occ
	occ.ResourceRevision = 4
	occ.ManifestHash = "manifest-hash"
	rt.resources = &fakeResources{contextText: "<task-resources>revision 4</task-resources>"}
	rec := seedAccepted(t, rt)

	rt.execute(ctx, *rec)
	req := turns.lastRequest()
	require.NotNil(t, req)
	assert.Equal(t, "<task-resources>revision 4</task-resources>", req.LLMContextPrefix)
	assert.Equal(t, "summarize the workspace", req.Content,
		"the stored Task.prompt stays the original text")
}

func TestExecutePauseRaceDoesNotStart(t *testing.T) {
	rt, store, occSource, turns, _ := runtimeFixture(t)
	ctx := context.Background()
	rec := seedAccepted(t, rt)
	occSource.authorize = func(AuthorizeStartRequest) (*Grant, error) {
		return nil, rpcx.Errorf(rpcx.CodeStartRejected, "task paused")
	}

	rt.execute(ctx, *rec)

	got, err := store.GetScheduledExecution(ctx, rec.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledReceiptError, got.Status)
	assert.Equal(t, model.ScheduledReasonStartRejected, got.Reason)
	assert.Equal(t, model.ScheduledStopProofNotStarted, got.StopProofKind)
	// The fake turn ran the preamble/fence only; no terminal callback, so no
	// content was ever produced.
	assert.Zero(t, turns.terminalCalls, "the turn must never produce a terminal fact after a definitive rejection")
}

func TestExecuteAmbiguousGrantBecomesUncertain(t *testing.T) {
	rt, store, occSource, turns, _ := runtimeFixture(t)
	ctx := context.Background()
	rec := seedAccepted(t, rt)
	occSource.authorize = func(AuthorizeStartRequest) (*Grant, error) {
		return nil, rpcx.Errorf(rpcx.CodeUnavailable, "scheduler offline")
	}
	occSource.getGrant = func(string, uint64) (*Grant, error) {
		return nil, rpcx.Errorf(rpcx.CodeUnavailable, "scheduler offline")
	}

	rt.execute(ctx, *rec)

	got, err := store.GetScheduledExecution(ctx, rec.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledReceiptUncertain, got.Status)
	assert.Equal(t, model.ScheduledReasonResultUnknown, got.Reason)
	assert.Zero(t, turns.terminalCalls, "an unknown grant never produces a terminal fact")
}

func TestExecuteRecoversLostGrantResponse(t *testing.T) {
	rt, store, occSource, turns, _ := runtimeFixture(t)
	ctx := context.Background()
	rec := seedAccepted(t, rt)
	occSource.authorize = func(req AuthorizeStartRequest) (*Grant, error) {
		return nil, rpcx.Errorf(rpcx.CodeUnavailable, "response lost")
	}
	occSource.getGrant = func(occurrenceID string, generation uint64) (*Grant, error) {
		return &Grant{
			OccurrenceID: occurrenceID, Generation: generation,
			RunID: "run-1", SessionID: "sess-1",
			StartedAt: time.Now().UTC(), Deadline: time.Now().Add(time.Hour).UTC(),
		}, nil
	}

	rt.execute(ctx, *rec)

	got, err := store.GetScheduledExecution(ctx, rec.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledReceiptSucceeded, got.Status)
	assert.NotNil(t, turns.lastRequest())
}

func TestExecuteWatchdogCancelsAndPreservesTimeoutFact(t *testing.T) {
	rt, store, occSource, turns, canceller := runtimeFixture(t)
	ctx := context.Background()
	rec := seedAccepted(t, rt)

	released := make(chan struct{})
	cancelled := make(chan string, 1)
	canceller.onCancel = func(runID string) { cancelled <- runID }
	occSource.authorize = func(req AuthorizeStartRequest) (*Grant, error) {
		return &Grant{
			OccurrenceID: req.OccurrenceID, Generation: req.ReceiptGeneration,
			RunID: req.RunID, SessionID: req.SessionID,
			StartedAt: time.Now().Add(-2 * time.Minute).UTC(),
			Deadline:  time.Now().Add(-time.Second).UTC(), // already expired
		}, nil
	}
	turns.beforeCall = func(*turn.Request) { <-released }
	go func() {
		<-cancelled
		close(released)
	}()

	rt.execute(ctx, *rec)

	got, err := store.GetScheduledExecution(ctx, rec.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledReceiptCancelled, got.Status)
	assert.Equal(t, model.ScheduledReasonTaskTimeout, got.Reason)
	assert.Equal(t, []string{"run-1"}, canceller.runs)
}

func TestExecuteDoesNotClaimSuccessWithoutDurableContent(t *testing.T) {
	rt, store, _, turns, _ := runtimeFixture(t)
	ctx := context.Background()
	rec := seedAccepted(t, rt)
	turns.terminal = turn.TerminalResult{
		RunID: "run-1", SessionID: "sess-1", UserID: "user-1",
		Status: "done", MessagesDurable: false,
	}

	rt.execute(ctx, *rec)

	got, err := store.GetScheduledExecution(ctx, rec.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledReceiptError, got.Status)
	assert.Equal(t, model.ScheduledReasonPersistFailed, got.Reason)
}

func TestReconcileMarksUncertainAndProvesStopped(t *testing.T) {
	rt, store, _, _, _ := runtimeFixture(t)
	ctx := context.Background()
	rec := seedAccepted(t, rt)
	now := time.Now().UTC()

	claimed, ok, err := store.ClaimScheduledReceipt(ctx, rec.OccurrenceID, "worker-a", time.Minute, 0, now)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = store.MarkScheduledReceiptStartIntent(ctx, rec.OccurrenceID, claimed.ReceiptGeneration, "worker-a", time.Minute, now)
	require.NoError(t, err)
	require.True(t, ok)
	// Age the start intent past the grace window.
	stored := store.receipts[rec.OccurrenceID]
	old := now.Add(-time.Hour)
	stored.StartIntentAt = &old

	rt.observer = &fakeObserver{alive: false}
	rt.reconcile(ctx)

	got, err := store.GetScheduledExecution(ctx, rec.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledReceiptUncertain, got.Status)

	ok, err = rt.ProveStopped(ctx, rec.OccurrenceID, got.ReceiptGeneration, "")
	require.NoError(t, err)
	require.True(t, ok)
	got, err = store.GetScheduledExecution(ctx, rec.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledReceiptInterrupted, got.Status)
	assert.Equal(t, model.ScheduledStopProofReconciled, got.StopProofKind)
}

func TestCancelPreStartAndRunning(t *testing.T) {
	rt, store, _, _, canceller := runtimeFixture(t)
	ctx := context.Background()
	rec := seedAccepted(t, rt)

	accepted, status, err := rt.CancelExecution(ctx, rec.OccurrenceID, "")
	require.NoError(t, err)
	assert.True(t, accepted)
	assert.Equal(t, model.ScheduledReceiptCancelled, status)

	// A started execution is cancelled through the run manager, not terminalized.
	store.receipts[rec.OccurrenceID].Status = model.ScheduledReceiptRunning
	store.receipts[rec.OccurrenceID].StopProofPresent = false
	accepted, status, err = rt.CancelExecution(ctx, rec.OccurrenceID, "")
	require.NoError(t, err)
	assert.True(t, accepted)
	assert.Equal(t, model.ScheduledReceiptRunning, status)
	assert.Equal(t, []string{"run-1"}, canceller.runs)
}

func mustCode(t *testing.T, err error) string {
	t.Helper()
	de, ok := rpcx.AsError(err)
	require.True(t, ok, "expected a domain error, got %v", err)
	return de.Code
}
