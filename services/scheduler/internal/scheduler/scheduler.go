// Package scheduler owns the standalone scheduling loop: durable
// materialization, claims, bounded submission retries, Runtime reconciliation,
// start-grant authorization and the task-level execution hold. It never
// constructs an Agent, a turn, or a user workspace.
package scheduler

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/rpcx"
	"github.com/lush/blowball/internal/scheduledcontract"
)

// Config bounds the standalone scheduler.
type Config struct {
	PollInterval   time.Duration
	LeaseDuration  time.Duration
	MaxRunDuration time.Duration
	MaxConcurrent  int
	ScanBatch      int
	MaxAttempts    int
	RetryBackoff   time.Duration
	// ExecutionEnabled is the deployment switch. Management RPCs remain
	// available when false; materialization/submission are off.
	ExecutionEnabled bool
}

func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = 30 * time.Second
	}
	if c.MaxRunDuration <= 0 {
		c.MaxRunDuration = 10 * time.Minute
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 4
	}
	if c.ScanBatch <= 0 || c.ScanBatch > 100 {
		c.ScanBatch = 100
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = time.Second
	}
	return c
}

// Store is the Scheduler-owned persistence surface.
type Store interface {
	MaterializeTasks(ctx context.Context, now time.Time, limit int) ([]model.ScheduledTaskRun, error)
	ListClaimableRuns(ctx context.Context, now time.Time, limit, maxAttempts int) ([]model.ScheduledTaskRun, error)
	ClaimRun(ctx context.Context, occurrenceID string, now time.Time, lease time.Duration, maxAttempts int) (*model.ScheduledTaskRun, bool, error)
	MarkLaunching(ctx context.Context, occurrenceID, claimToken string, now time.Time, lease time.Duration) (bool, error)
	SetReceiptGeneration(ctx context.Context, occurrenceID string, generation uint64) error
	ScheduleRetry(ctx context.Context, occurrenceID, claimToken, reason, lastError string, next time.Time) (bool, error)
	RequeueRun(ctx context.Context, occurrenceID, reason, lastError string, next time.Time) (bool, error)
	FinishBeforeStart(ctx context.Context, occurrenceID, claimToken, status, reason, lastError string, now time.Time) (bool, error)
	FinishExhausted(ctx context.Context, occurrenceID, status, reason, lastError string, now time.Time) (bool, error)
	FinishOccurrence(ctx context.Context, occurrenceID, status, reason, lastError string, receiptGeneration uint64, now time.Time) (bool, error)
	MarkRunningFromReceipt(ctx context.Context, occurrenceID string, receiptGeneration uint64, startedAt *time.Time) error
	MarkInterruptedFromReceipt(ctx context.Context, occurrenceID string, receiptGeneration uint64, lastError string, now time.Time) (bool, error)
	ListRecoverableRuns(ctx context.Context, limit int) ([]model.ScheduledTaskRun, error)
	ListExhaustedRuns(ctx context.Context, maxAttempts, limit int) ([]model.ScheduledTaskRun, error)
	SetHold(ctx context.Context, taskID, occurrenceID, reason string, now time.Time) error
	ClearHold(ctx context.Context, taskID, occurrenceID string) (bool, error)
	ListHeldOccurrences(ctx context.Context, limit int) ([]model.ScheduledTaskRun, error)
	GetTaskInternal(ctx context.Context, taskID string) (*model.ScheduledTask, error)
	PauseTask(ctx context.Context, taskID, reason string, now time.Time) error
	AuthorizeStart(ctx context.Context, occurrenceID, runID, sessionID string, attempt int, receiptGeneration uint64, workerGeneration string, now time.Time, duration time.Duration) (*model.ScheduledStartGrant, error)
	GetStartGrant(ctx context.Context, occurrenceID string, receiptGeneration uint64) (*model.ScheduledStartGrant, error)
}

// SubmitInput is the idempotent acceptance request.
type SubmitInput struct {
	OccurrenceID      string
	TaskID            string
	RunID             string
	SessionID         string
	Attempt           int
	ConfigFingerprint string
}

// SubmitResult is the durable acceptance.
type SubmitResult struct {
	ReceiptGeneration uint64
	Status            string
	AlreadyAccepted   bool
}

// ExecutionFact is the authoritative Runtime fact projection.
type ExecutionFact struct {
	Status              string
	Reason              string
	LastError           string
	ReceiptGeneration   uint64
	Terminal            bool
	Started             bool
	Uncertain           bool
	StartedAt           *time.Time
	FinishedAt          *time.Time
	GrantGeneration     uint64
	GrantDeadline       *time.Time
	StopProofPresent    bool
	StopProofGeneration uint64
}

// RuntimeClient is the Runtime-owned execution RPC surface.
type RuntimeClient interface {
	SubmitScheduledRun(ctx context.Context, in SubmitInput) (*SubmitResult, error)
	GetScheduledExecution(ctx context.Context, occurrenceID string) (*ExecutionFact, error)
}

// Scheduler is one standalone scheduler instance.
type Scheduler struct {
	store   Store
	runtime RuntimeClient
	cfg     Config
	now     func() time.Time

	startOnce sync.Once
	stopOnce  sync.Once
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	sem       chan struct{}
	activeMu  sync.Mutex
	active    map[string]struct{}
}

// New builds a scheduler instance.
func New(store Store, runtimeClient RuntimeClient, cfg Config, clocks ...func() time.Time) *Scheduler {
	now := time.Now
	if len(clocks) > 0 && clocks[0] != nil {
		now = clocks[0]
	}
	cfg = cfg.withDefaults()
	return &Scheduler{
		store: store, runtime: runtimeClient, cfg: cfg, now: now,
		sem: make(chan struct{}, cfg.MaxConcurrent), active: map[string]struct{}{},
	}
}

// Start launches the polling loop.
func (s *Scheduler) Start() {
	s.startOnce.Do(func() {
		s.ctx, s.cancel = context.WithCancel(context.Background())
		if !s.cfg.ExecutionEnabled {
			return
		}
		s.wg.Add(1)
		go s.loop()
	})
}

func (s *Scheduler) loop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	s.RunOnce(s.ctx)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.RunOnce(s.ctx)
		}
	}
}

// Shutdown stops claiming/materializing and waits for in-flight RPC workers.
// It does not cancel any execution already accepted by the Runtime.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RunOnce performs one bounded cycle. It is exported for tests.
func (s *Scheduler) RunOnce(ctx context.Context) error {
	if !s.cfg.ExecutionEnabled || ctx.Err() != nil {
		return nil
	}
	var errs []error
	if _, err := s.store.MaterializeTasks(ctx, s.now().UTC(), s.cfg.ScanBatch); err != nil {
		errs = append(errs, err)
	}
	if err := s.exhaustRetries(ctx); err != nil {
		errs = append(errs, err)
	}
	runs, err := s.store.ListClaimableRuns(ctx, s.now().UTC(), s.cfg.ScanBatch, s.cfg.MaxAttempts)
	if err != nil {
		errs = append(errs, err)
	} else {
		for i := range runs {
			if !s.dispatch(&runs[i]) {
				break
			}
		}
	}
	if err := s.reconcile(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *Scheduler) dispatch(run *model.ScheduledTaskRun) bool {
	if ctxDone(s.ctx) {
		return false
	}
	if !s.beginWorker(run.OccurrenceID) {
		return false
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.endWorker(run.OccurrenceID)
		s.execute(s.ctx, run)
	}()
	return true
}

func (s *Scheduler) beginWorker(occurrenceID string) bool {
	select {
	case s.sem <- struct{}{}:
	default:
		return false
	}
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if _, exists := s.active[occurrenceID]; exists {
		<-s.sem
		return false
	}
	s.active[occurrenceID] = struct{}{}
	return true
}

func (s *Scheduler) endWorker(occurrenceID string) {
	s.activeMu.Lock()
	delete(s.active, occurrenceID)
	s.activeMu.Unlock()
	<-s.sem
}

// execute claims a due occurrence, persists launching, and submits the stable
// identity to the Runtime. It never invokes the Agent and never waits for the
// execution to finish.
func (s *Scheduler) execute(ctx context.Context, candidate *model.ScheduledTaskRun) {
	claimed, ok, err := s.store.ClaimRun(ctx, candidate.OccurrenceID, s.now().UTC(), s.cfg.LeaseDuration, s.cfg.MaxAttempts)
	if err != nil {
		s.warn(ctx, "scheduled occurrence claim failed", err)
		return
	}
	if !ok {
		return
	}
	run := claimed
	if ok, err := s.store.MarkLaunching(ctx, run.OccurrenceID, run.ClaimToken, s.now().UTC(), s.cfg.LeaseDuration); err != nil || !ok {
		if err != nil {
			s.warn(ctx, "scheduled launching persist failed", err)
		}
		return
	}
	fingerprint := scheduledcontract.Fingerprint(scheduledcontract.FingerprintInput{
		TaskID: run.TaskID, OccurrenceID: run.OccurrenceID, RunID: run.RunID,
		SessionID: run.SessionID, SnapshotJSON: run.SnapshotJSON,
		ResourceRevision: run.ResourceRevision, ManifestHash: run.ManifestHash,
	})
	result, err := s.runtime.SubmitScheduledRun(ctx, SubmitInput{
		OccurrenceID: run.OccurrenceID, TaskID: run.TaskID, RunID: run.RunID,
		SessionID: run.SessionID, Attempt: run.AttemptCount, ConfigFingerprint: fingerprint,
	})
	if err != nil {
		s.handleSubmitFailure(ctx, run, err)
		return
	}
	if err := s.store.SetReceiptGeneration(ctx, run.OccurrenceID, result.ReceiptGeneration); err != nil {
		s.warn(ctx, "scheduled receipt generation persist failed", err)
	}
}

// handleSubmitFailure distinguishes definitive rejection from an ambiguous
// transport failure. Ambiguity is resolved by reading the authoritative
// receipt; if it does not exist, a bounded retry re-submits the SAME identity.
func (s *Scheduler) handleSubmitFailure(ctx context.Context, run *model.ScheduledTaskRun, cause error) {
	fact, factErr := s.runtime.GetScheduledExecution(ctx, run.OccurrenceID)
	if factErr == nil && fact != nil {
		_ = s.store.SetReceiptGeneration(ctx, run.OccurrenceID, fact.ReceiptGeneration)
		return // accepted after all; reconciliation tracks it
	}
	if !ambiguous(cause) {
		status, reason := model.ScheduledRunError, model.ScheduledReasonStartRejected
		if de, ok := rpcx.AsError(cause); ok {
			switch de.Code {
			case rpcx.CodeUserUnavailable:
				reason = model.ScheduledReasonUserUnavailable
			case rpcx.CodeSessionPolicy:
				status, reason = model.ScheduledRunSkipped, model.ScheduledSessionPolicyMigrationReason
			case rpcx.CodeSessionBusy:
				status, reason = model.ScheduledRunSkipped, model.ScheduledReasonSessionBusy
			}
		}
		_, _ = s.store.FinishBeforeStart(ctx, run.OccurrenceID, run.ClaimToken,
			status, reason, cause.Error(), s.now().UTC())
		if reason == model.ScheduledReasonUserUnavailable {
			if perr := s.store.PauseTask(ctx, run.TaskID, reason, s.now().UTC()); perr != nil {
				s.warn(ctx, "pause unavailable-owner task failed", perr)
			}
		}
		return
	}
	// The runtime is unreachable and no receipt was found. Requeue within the
	// AttemptCount budget; ClaimRun refuses once the budget is exhausted and
	// exhaustRetries terminalizes it.
	retried, retryErr := s.store.ScheduleRetry(ctx, run.OccurrenceID, run.ClaimToken,
		model.ScheduledReasonAdmission, cause.Error(), s.now().UTC().Add(s.cfg.RetryBackoff))
	if retryErr != nil || !retried {
		_, _ = s.store.RequeueRun(ctx, run.OccurrenceID, model.ScheduledReasonAdmission, cause.Error(),
			s.now().UTC().Add(s.cfg.RetryBackoff))
	}
}

// reconcile projects authoritative Runtime facts, sets/clears the task hold,
// and terminalizes exhausted submission attempts.
func (s *Scheduler) reconcile(ctx context.Context) error {
	var errs []error
	runs, err := s.store.ListRecoverableRuns(ctx, s.cfg.ScanBatch)
	if err != nil {
		return err
	}
	for i := range runs {
		run := &runs[i]
		fact, err := s.runtime.GetScheduledExecution(ctx, run.OccurrenceID)
		if err != nil {
			// An unreachable Runtime is not evidence of anything. Keep the
			// existing state; the hold/uncertain window is entered only from a
			// trustworthy fact or an expired execution deadline.
			if run.GrantGeneration > 0 && s.deadlinePassed(run) {
				s.markUnknown(ctx, run, "runtime unavailable after the grant deadline")
			}
			continue
		}
		if fact == nil {
			// No receipt. Before any grant this is safe to retry; after a grant
			// it is an inconsistency and must be treated conservatively.
			if run.GrantGeneration > 0 {
				s.markUnknown(ctx, run, "receipt missing after start grant")
			} else if run.Status == model.ScheduledRunLaunching {
				_, _ = s.store.RequeueRun(ctx, run.OccurrenceID, model.ScheduledReasonAdmission,
					"runtime receipt not found", s.now().UTC().Add(s.cfg.RetryBackoff))
			}
			continue
		}
		_ = s.store.SetReceiptGeneration(ctx, run.OccurrenceID, fact.ReceiptGeneration)
		switch {
		case fact.Terminal:
			status := occurrenceStatus(fact.Status)
			if _, err := s.store.FinishOccurrence(ctx, run.OccurrenceID, status, fact.Reason, fact.LastError, fact.ReceiptGeneration, s.now().UTC()); err != nil {
				errs = append(errs, err)
			}
			if fact.StopProofPresent {
				_, _ = s.store.ClearHold(ctx, run.TaskID, run.OccurrenceID)
			}
			if fact.Reason == model.ScheduledReasonUserUnavailable {
				if err := s.store.PauseTask(ctx, run.TaskID, model.ScheduledReasonUserUnavailable, s.now().UTC()); err != nil {
					errs = append(errs, err)
				}
			}
		case fact.Uncertain:
			s.markUnknown(ctx, run, fact.LastError)
		case fact.Started:
			if err := s.store.MarkRunningFromReceipt(ctx, run.OccurrenceID, fact.ReceiptGeneration, fact.StartedAt); err != nil {
				errs = append(errs, err)
			}
		}
		if fact.Terminal {
			continue
		}
		// A grant deadline that passed without a terminal fact is a
		// conservative unknown: the Runtime may still be running.
		if run.GrantGeneration > 0 && fact.GrantDeadline != nil && s.now().UTC().After(fact.GrantDeadline.Add(2*s.cfg.LeaseDuration)) {
			s.markUnknown(ctx, run, "grant deadline passed without a terminal fact")
		}
	}
	// Clear holds whose owning execution has since produced a stop proof.
	held, err := s.store.ListHeldOccurrences(ctx, s.cfg.ScanBatch)
	if err == nil {
		for i := range held {
			run := &held[i]
			fact, ferr := s.runtime.GetScheduledExecution(ctx, run.OccurrenceID)
			if ferr != nil || fact == nil || !fact.Terminal || !fact.StopProofPresent {
				continue
			}
			_, _ = s.store.ClearHold(ctx, run.TaskID, run.OccurrenceID)
		}
	} else {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *Scheduler) markUnknown(ctx context.Context, run *model.ScheduledTaskRun, reason string) {
	// Keep the occurrence active until its durable task hold blocks later
	// slots. A failed hold write must remain eligible for reconciliation.
	if err := s.store.SetHold(ctx, run.TaskID, run.OccurrenceID, model.ScheduledReasonResultUnknown, s.now().UTC()); err != nil {
		s.warn(ctx, "scheduled execution hold persist failed", err)
		return
	}
	if _, err := s.store.MarkInterruptedFromReceipt(ctx, run.OccurrenceID, run.ReceiptGeneration, reason, s.now().UTC()); err != nil {
		s.warn(ctx, "scheduled interrupted projection failed", err)
	}
}

func (s *Scheduler) deadlinePassed(run *model.ScheduledTaskRun) bool {
	return run.GrantDeadline != nil && s.now().UTC().After(run.GrantDeadline.Add(2*s.cfg.LeaseDuration))
}

// exhaustRetries terminalizes occurrences whose bounded preparation attempts
// are used up without ever crossing a grant.
func (s *Scheduler) exhaustRetries(ctx context.Context) error {
	runs, err := s.store.ListExhaustedRuns(ctx, s.cfg.MaxAttempts, s.cfg.ScanBatch)
	if err != nil {
		return err
	}
	for i := range runs {
		run := &runs[i]
		_, _ = s.store.FinishExhausted(ctx, run.OccurrenceID,
			model.ScheduledRunError, model.ScheduledReasonStartRejected, "preparation retry budget exhausted", s.now().UTC())
	}
	return nil
}

// AuthorizeStart implements the Runtime's cross-service startup fence.
func (s *Scheduler) AuthorizeStart(ctx context.Context, occurrenceID, runID, sessionID string, attempt int, receiptGeneration uint64, workerGeneration string) (*model.ScheduledStartGrant, error) {
	return s.store.AuthorizeStart(ctx, occurrenceID, runID, sessionID, attempt, receiptGeneration, workerGeneration, s.now().UTC(), s.cfg.MaxRunDuration)
}

// GetStartGrant resolves an idempotent grant after an ambiguous response.
func (s *Scheduler) GetStartGrant(ctx context.Context, occurrenceID string, receiptGeneration uint64) (*model.ScheduledStartGrant, error) {
	return s.store.GetStartGrant(ctx, occurrenceID, receiptGeneration)
}

func occurrenceStatus(receiptStatus string) string {
	switch receiptStatus {
	case model.ScheduledReceiptSucceeded:
		return model.ScheduledRunSucceeded
	case model.ScheduledReceiptCancelled:
		return model.ScheduledRunCancelled
	case model.ScheduledReceiptSkipped:
		return model.ScheduledRunSkipped
	case model.ScheduledReceiptInterrupted:
		return model.ScheduledRunInterrupted
	default:
		return model.ScheduledRunError
	}
}

func ambiguous(err error) bool { return !definitive(err) }

// definitive reports whether the Runtime rejected the submission as a
// permanent domain error. Anything else (transport failures, deadlines,
// internal errors) leaves the acceptance outcome unknown.
func definitive(err error) bool {
	de, ok := rpcx.AsError(err)
	if !ok {
		return false
	}
	switch de.Code {
	case rpcx.CodeInvalidArgument, rpcx.CodeInvalidModel, rpcx.CodeInvalidEffort,
		rpcx.CodeNotFound, rpcx.CodeConflict, rpcx.CodeFailedPrecondition,
		rpcx.CodePermissionDenied, rpcx.CodeUnauthenticated,
		rpcx.CodeSessionPolicy, rpcx.CodeUserUnavailable, rpcx.CodeStartRejected:
		return true
	default:
		return false
	}
}

func ctxDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func (s *Scheduler) warn(ctx context.Context, msg string, err error) {
	logger.FromContext(ctx).Warn(msg, zap.String("op", "scheduler"), zap.Error(err))
}
