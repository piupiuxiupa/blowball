package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/modelselect"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/turn"
)

var (
	errFenceRejected  = errors.New("scheduled start fence rejected")
	errFenceUncertain = errors.New("scheduled start fence outcome unknown")
)

func (s *Scheduler) execute(ctx context.Context, candidate *model.ScheduledTaskRun) {
	claimed, ok, err := s.store.ClaimScheduledRun(ctx, candidate.OccurrenceID, s.now().UTC(), s.cfg.LeaseDuration, s.cfg.MaxAttempts)
	if err != nil {
		logger.L().Warn("scheduled occurrence claim failed", zap.String("op", "scheduler.claim"), zap.String("occurrence_id", candidate.OccurrenceID), zap.Error(err))
		return
	}
	if !ok {
		return
	}
	run := claimed
	if reason, err := s.store.ScheduledRunAuthorization(ctx, run.OccurrenceID); err != nil {
		s.retryOrFinish(run, model.ScheduledReasonStartRejected, err)
		return
	} else if reason != "" {
		s.finishAuthorizationFailure(run, reason, nil)
		return
	}

	snapshot, err := run.Snapshot()
	if err != nil {
		s.finishBeforeStart(run, model.ScheduledRunError, model.ScheduledReasonStartRejected, err)
		return
	}
	selection, code, message := modelselect.Resolve(s.selection, snapshot.Model, snapshot.ReasoningEffort)
	if code != "" {
		s.finishBeforeStart(run, model.ScheduledRunError, model.ScheduledReasonStartRejected, fmt.Errorf("%s: %s", code, message))
		return
	}

	sess, err := s.sessions.EnsureSessionWithID(ctx, run.UserID, run.SessionID, run.RunID)
	if err != nil {
		if reason, authErr := s.store.ScheduledRunAuthorization(context.Background(), run.OccurrenceID); authErr == nil && reason != "" {
			s.finishAuthorizationFailure(run, reason, err)
			return
		}
		s.retryOrFinish(run, model.ScheduledReasonStartRejected, err)
		return
	}
	launched, err := s.store.MarkScheduledRunLaunching(ctx, run.OccurrenceID, run.ClaimToken, s.now().UTC())
	if err != nil {
		s.retryOrFinish(run, model.ScheduledReasonStartRejected, err)
		return
	}
	if !launched {
		s.handleDefiniteRejection(run, nil)
		return
	}

	token, err := s.tokens.Issue(ctx, run.UserID)
	if err != nil {
		s.retryOrFinish(run, model.ScheduledReasonStartRejected, err)
		return
	}

	var timerMu sync.Mutex
	var timeout *time.Timer
	var timedOut atomic.Bool
	req := &turn.Request{
		SessionID: run.SessionID, UserID: run.UserID, RunID: run.RunID,
		Content: snapshot.Prompt, Selection: turnSelection(selection), RawToken: token,
		UserMsgTime: s.now().UTC(), ContextCompacted: sess.ContextCompacted, MetaCreatedAt: run.CreateTime,
	}
	req.BeforeStart = func(fenceCtx context.Context) error {
		if ctx.Err() != nil {
			return turn.ErrShutdownDuringStartup
		}
		crossed, err := s.store.CrossScheduledStartFence(fenceCtx, run.OccurrenceID, run.ClaimToken, s.now().UTC())
		if err != nil {
			return fmt.Errorf("%w: %v", errFenceUncertain, err)
		}
		if !crossed {
			return errFenceRejected
		}
		timerMu.Lock()
		timeout = time.AfterFunc(s.cfg.MaxRunDuration, func() {
			timedOut.Store(true)
			s.cancelRun(run.RunID)
		})
		timerMu.Unlock()
		return nil
	}
	req.OnTerminal = func(_ context.Context, result turn.TerminalResult) {
		timerMu.Lock()
		if timeout != nil {
			timeout.Stop()
		}
		timerMu.Unlock()
		status := scheduledTerminalStatus(result.Status)
		reason := ""
		if timedOut.Load() {
			reason = model.ScheduledReasonTaskTimeout
		}
		s.finishTerminalWithRetry(run.OccurrenceID, status, reason, result.Err)
	}

	submission := s.turns.Submit(scheduledContext(ctx, run), req)
	switch submission.Outcome {
	case turn.SubmitStarted:
		select {
		case <-submission.Started.ResultCh:
			// OnTerminal has already run after durable message persistence.
		case <-ctx.Done():
			s.cancelRun(run.RunID)
			<-submission.Started.ResultCh
		}
	case turn.SubmitSessionBusy:
		s.finishBeforeStart(run, model.ScheduledRunSkipped, model.ScheduledReasonSessionBusy, submission.Err)
	case turn.SubmitAdmissionFailed:
		s.retryOrFinish(run, model.ScheduledReasonAdmission, submission.Err)
	case turn.SubmitShutdown:
		s.retryOrFinish(run, model.ScheduledReasonStartRejected, submission.Err)
	case turn.SubmitRejected:
		switch {
		case errors.Is(submission.Err, errFenceUncertain):
			s.finishTerminalWithRetry(run.OccurrenceID, model.ScheduledRunInterrupted, model.ScheduledReasonResultUnknown, submission.Err)
		case errors.Is(submission.Err, turn.ErrShutdownDuringStartup):
			s.retryOrFinish(run, model.ScheduledReasonStartRejected, submission.Err)
		case errors.Is(submission.Err, errFenceRejected):
			s.handleDefiniteRejection(run, submission.Err)
		default:
			s.retryOrFinish(run, model.ScheduledReasonStartRejected, submission.Err)
		}
	}
}

func turnSelection(selection modelselect.Selection) turn.Selection {
	return turn.Selection{
		Model: selection.Model, Thinking: selection.Thinking, Vision: selection.Vision,
		Effort: selection.Effort, MaxContextTokens: selection.MaxContextTokens,
		MaxCompletionTokens: selection.MaxCompletionTokens, LengthContinue: selection.LengthContinue,
	}
}

func (s *Scheduler) handleDefiniteRejection(run *model.ScheduledTaskRun, cause error) {
	reason, err := s.store.ScheduledRunAuthorization(context.Background(), run.OccurrenceID)
	if err == nil && reason != "" {
		s.finishAuthorizationFailure(run, reason, cause)
		return
	}
	s.retryOrFinish(run, model.ScheduledReasonStartRejected, cause)
}

func (s *Scheduler) finishAuthorizationFailure(run *model.ScheduledTaskRun, reason string, cause error) {
	status := model.ScheduledRunSkipped
	if reason == model.ScheduledReasonTaskPaused || reason == model.ScheduledReasonTaskDeleted {
		status = model.ScheduledRunCancelled
	}
	s.finishBeforeStart(run, status, reason, cause)
	if reason == model.ScheduledReasonUserUnavailable || reason == model.ScheduledReasonSessionUnavailable {
		if err := s.store.PauseScheduledTask(context.Background(), run.TaskID, reason, s.now().UTC()); err != nil {
			logger.L().Warn("pause after authorization failure failed", zap.String("op", "scheduler.pause"), zap.String("task_id", run.TaskID), zap.Error(err))
		}
	}
}

func (s *Scheduler) finishBeforeStart(run *model.ScheduledTaskRun, status, reason string, cause error) {
	_, _ = s.store.FinishScheduledRunBeforeStart(context.Background(), run.OccurrenceID, run.ClaimToken,
		status, reason, scheduledError(cause), s.now().UTC())
}

func (s *Scheduler) retryOrFinish(run *model.ScheduledTaskRun, reason string, cause error) {
	if run.AttemptCount >= s.cfg.MaxAttempts {
		s.finishBeforeStart(run, model.ScheduledRunError, reason, cause)
		return
	}
	backoff := s.cfg.RetryBackoff
	for i := 1; i < run.AttemptCount && backoff < 30*time.Second; i++ {
		backoff *= 2
	}
	if backoff > 30*time.Second {
		backoff = 30 * time.Second
	}
	if _, err := s.store.ScheduleScheduledRunRetry(context.Background(), run.OccurrenceID, run.ClaimToken,
		s.now().UTC().Add(backoff), scheduledError(cause)); err != nil {
		logger.L().Warn("schedule safe retry failed", zap.String("op", "scheduler.retry"), zap.String("occurrence_id", run.OccurrenceID), zap.Error(err))
	}
}

func (s *Scheduler) finishTerminalWithRetry(occurrenceID, status, reason string, cause error) {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var updated bool
		updated, err = s.store.FinishScheduledRun(ctx, occurrenceID, status, reason, scheduledError(cause), s.now().UTC())
		cancel()
		if err == nil {
			if !updated {
				logger.L().Warn("scheduled terminal update was superseded", zap.String("op", "scheduler.finish"), zap.String("occurrence_id", occurrenceID))
			}
			return
		}
		if attempt < 3 {
			time.Sleep(s.cfg.RetryBackoff)
		}
	}
	logger.L().Error("scheduled terminal update failed after retries; recovery will reconcile",
		zap.String("op", "scheduler.finish"), zap.String("occurrence_id", occurrenceID), zap.Error(err))
}

func scheduledTerminalStatus(status string) string {
	switch status {
	case run.StatusDone:
		return model.ScheduledRunSucceeded
	case run.StatusCancelled:
		return model.ScheduledRunCancelled
	case run.StatusInterrupted:
		return model.ScheduledRunInterrupted
	default:
		return model.ScheduledRunError
	}
}

func (s *Scheduler) cancelRun(runID string) {
	if s.runs == nil {
		return
	}
	if s.runs.Registry != nil && s.runs.Registry.Cancel(runID) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.runs.Store.SetCancelFlag(ctx, runID); err != nil {
		logger.L().Warn("scheduled run cancel flag failed", zap.String("op", "scheduler.cancel"), zap.String("run_id", runID), zap.Error(err))
	}
}

func scheduledError(err error) string {
	if err == nil {
		return ""
	}
	runes := []rune(err.Error())
	if len(runes) > 4096 {
		runes = runes[:4096]
	}
	return string(runes)
}

// Keep the request context constructors in one place for tests and recovery.
func scheduledContext(ctx context.Context, run *model.ScheduledTaskRun) context.Context {
	return agent.WithUserID(agent.WithSessionID(trace.WithContext(ctx, run.RunID), run.SessionID), run.UserID)
}
