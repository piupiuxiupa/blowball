package scheduledruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/modelselect"
	"github.com/lush/blowball/pkg/logger"
	"github.com/lush/blowball/pkg/rpcx"
	"github.com/lush/blowball/pkg/run"
	"github.com/lush/blowball/pkg/turn"
)

var errFenced = errors.New("scheduled execution generation fenced")

// execute claims one receipt and drives it through the pre-start validation,
// start-intent/authorization fence, the shared turn, and the terminal fact.
func (r *Runtime) execute(ctx context.Context, candidate model.ScheduledExecutionReceipt) {
	workerGeneration := r.workerID + ":" + randomID()
	claimed, ok, err := r.store.ClaimScheduledReceipt(ctx, candidate.OccurrenceID, workerGeneration, r.cfg.LeaseDuration, 0, r.now())
	if err != nil {
		logWarn(ctx, "scheduled receipt claim failed", err)
		return
	}
	if !ok {
		return
	}
	rec := *claimed
	if rec.RuntimeAttempts >= r.cfg.MaxPrepareAttempts {
		r.finishForError(ctx, rec, workerGeneration,
			rpcx.Errorf(rpcx.CodeStartRejected, "preparation attempt cap reached"))
		return
	}

	occ, snap, sel, resourceContext, err := r.loadAuthoritative(ctx, rec.OccurrenceID)
	if err != nil {
		r.finishForError(ctx, rec, workerGeneration, err)
		return
	}
	if err := verifyReceiptIdentity(occ, rec); err != nil {
		r.finishForError(ctx, rec, workerGeneration, err)
		return
	}
	token, err := r.tokens.Issue(ctx, rec.UserID)
	if err != nil {
		r.finishForError(ctx, rec, workerGeneration,
			rpcx.Wrap(rpcx.CodeUnavailable, "execution identity issue failed", err))
		return
	}

	var timedOut atomic.Bool
	var timerMu sync.Mutex
	var watchdog *time.Timer

	req := &turn.Request{
		SessionID:        rec.SessionID,
		UserID:           rec.UserID,
		RunID:            rec.RunID,
		Content:          snap.Prompt,
		Selection:        turnSelection(sel),
		RawToken:         token,
		UserMsgTime:      r.now().UTC(),
		ContextCompacted: false, // one turn per occurrence, always a fresh session
		MetaCreatedAt:    rec.AcceptedAt,
		Source:           model.SessionSourceScheduled,
		LLMContextPrefix: resourceContext,
	}
	req.BeforeStart = func(fenceCtx context.Context) error {
		started, err := r.store.MarkScheduledReceiptStartIntent(fenceCtx, rec.OccurrenceID, rec.ReceiptGeneration,
			workerGeneration, r.cfg.LeaseDuration, r.now())
		if err != nil {
			return rpcx.Wrap(rpcx.CodeUnavailable, "start intent persist failed", err)
		}
		if !started {
			return errFenced
		}
		grant, err := r.authorizeStart(fenceCtx, occ, rec, workerGeneration)
		if err != nil {
			detached, cancel := detachedCtx(fenceCtx)
			defer cancel()
			if isAmbiguous(err) {
				// The grant outcome is unknown: the execution might have been
				// authorized. Conservative state; never replayed.
				_, _ = r.store.MarkScheduledReceiptUncertain(detached, rec.OccurrenceID, rec.ReceiptGeneration,
					model.ScheduledReasonResultUnknown, errText(err), r.now())
			} else {
				// Definitive rejection: the Scheduler transaction created no
				// grant, so no side effect can have begun.
				status, reason := startRejection(err)
				_, _ = r.store.FinishScheduledReceiptStartRejected(detached, rec.OccurrenceID, rec.ReceiptGeneration,
					workerGeneration, status, reason, errText(err), r.now())
			}
			return err
		}
		running, err := r.store.MarkScheduledReceiptRunning(fenceCtx, rec.OccurrenceID, rec.ReceiptGeneration,
			workerGeneration, model.ScheduledStartGrant{
				OccurrenceID: grant.OccurrenceID,
				Generation:   grant.Generation,
				RunID:        grant.RunID,
				SessionID:    grant.SessionID,
				StartedAt:    grant.StartedAt,
				Deadline:     grant.Deadline,
			})
		if err != nil {
			return rpcx.Wrap(rpcx.CodeUnavailable, "running fence persist failed", err)
		}
		if !running {
			return errFenced
		}
		deadline := grant.Deadline
		if deadline.IsZero() {
			deadline = r.now().Add(r.cfg.DefaultRunDuration)
		}
		timerMu.Lock()
		watchdog = time.AfterFunc(time.Until(deadline), func() {
			timedOut.Store(true)
			cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := r.canceller.RequestCancel(cancelCtx, rec.RunID); err != nil {
				logWarn(cancelCtx, "scheduled watchdog cancel request failed", err)
			}
		})
		timerMu.Unlock()
		return nil
	}
	req.OnTerminal = func(termCtx context.Context, res turn.TerminalResult) {
		timerMu.Lock()
		if watchdog != nil {
			watchdog.Stop()
		}
		timerMu.Unlock()
		status, reason := terminalFact(res, timedOut.Load())
		ok, err := r.store.FinishScheduledReceipt(termCtx, rec.OccurrenceID, rec.ReceiptGeneration,
			workerGeneration, status, reason, errText(res.Err), r.now())
		if err != nil {
			logWarn(termCtx, "scheduled terminal fact persist failed", err)
			return
		}
		if !ok {
			logWarn(termCtx, "scheduled terminal fact ignored: generation was fenced",
				errFenced)
			return
		}
		logger.FromContext(termCtx).Info("scheduled execution finished",
			zap.String("op", "scheduledruntime.terminal"),
			zap.String("occurrence_id", rec.OccurrenceID),
			zap.String("run_id", rec.RunID),
			zap.String("status", status),
			zap.Bool("messages_durable", res.MessagesDurable))
	}

	sub := r.turns.Submit(ctx, req)
	switch sub.Outcome {
	case turn.SubmitStarted:
		select {
		case <-sub.Started.ResultCh:
		case <-ctx.Done():
		}
	case turn.SubmitShutdown:
		detached, cancel := detachedCtx(ctx)
		defer cancel()
		if _, err := r.store.ReleaseScheduledReceiptOnShutdown(detached, rec.OccurrenceID, rec.ReceiptGeneration, workerGeneration); err != nil {
			logWarn(detached, "scheduled receipt shutdown release failed", err)
		}
	case turn.SubmitSessionBusy:
		r.releaseForRetry(ctx, rec, workerGeneration, model.ScheduledReasonSessionBusy, sub.Err)
	case turn.SubmitAdmissionFailed:
		r.releaseForRetry(ctx, rec, workerGeneration, model.ScheduledReasonAdmission, sub.Err)
	case turn.SubmitRejected:
		r.releaseForRetry(ctx, rec, workerGeneration, model.ScheduledReasonStartRejected, sub.Err)
	}
}

// authorizeStart asks the Scheduler for the unique grant. An ambiguous
// transport failure is resolved by reading the same grant identity; if that
// also fails, the caller records uncertainty and does not start.
func (r *Runtime) authorizeStart(ctx context.Context, occ *Occurrence, rec model.ScheduledExecutionReceipt, workerGeneration string) (*Grant, error) {
	grant, err := r.occurrences.AuthorizeStart(ctx, AuthorizeStartRequest{
		OccurrenceID:            occ.OccurrenceID,
		RunID:                   occ.RunID,
		SessionID:               occ.SessionID,
		Attempt:                 occ.AttemptCount,
		ReceiptGeneration:       rec.ReceiptGeneration,
		RuntimeWorkerGeneration: workerGeneration,
	})
	if err == nil {
		return grant, nil
	}
	if !isAmbiguous(err) {
		return nil, err
	}
	existing, gerr := r.occurrences.GetStartGrant(ctx, occ.OccurrenceID, rec.ReceiptGeneration)
	if gerr == nil && existing != nil {
		return existing, nil
	}
	return nil, err
}

func (r *Runtime) finishForError(ctx context.Context, rec model.ScheduledExecutionReceipt, workerGeneration string, cause error) {
	status, reason := startRejection(cause)
	detached, cancel := detachedCtx(ctx)
	defer cancel()
	ok, err := r.store.FinishScheduledReceiptBeforeStart(detached, rec.OccurrenceID, rec.ReceiptGeneration,
		workerGeneration, status, reason, errText(cause), r.now())
	if err != nil {
		logWarn(detached, "scheduled pre-start terminal fact persist failed", err)
		return
	}
	if !ok {
		logWarn(detached, "scheduled pre-start terminal fact ignored: generation was fenced", errFenced)
	}
}

func (r *Runtime) releaseForRetry(ctx context.Context, rec model.ScheduledExecutionReceipt, workerGeneration, reason string, cause error) {
	detached, cancel := detachedCtx(ctx)
	defer cancel()
	if _, err := r.store.ReleaseScheduledReceiptForRetry(detached, rec.OccurrenceID, rec.ReceiptGeneration,
		workerGeneration, reason, errText(cause), r.cfg.MaxPrepareAttempts, r.now()); err != nil {
		logWarn(detached, "scheduled receipt retry release failed", err)
	}
}

// verifyReceiptIdentity re-checks that the authoritative occurrence still
// matches the receipt's execution identity and configuration. This is the
// receiver-side defense against a forged or stale submission body.
func verifyReceiptIdentity(occ *Occurrence, rec model.ScheduledExecutionReceipt) error {
	if occ.OccurrenceID != rec.OccurrenceID || occ.TaskID != rec.TaskID ||
		occ.RunID != rec.RunID || occ.SessionID != rec.SessionID || occ.UserID != rec.UserID {
		return rpcx.Errorf(rpcx.CodeConflict, "authoritative occurrence identity does not match the receipt")
	}
	if fingerprintOf(occ) != rec.ConfigFingerprint {
		return rpcx.Errorf(rpcx.CodeConflict, "authoritative configuration no longer matches the receipt fingerprint")
	}
	return nil
}

// isAmbiguous reports whether a start-authorization failure leaves the grant
// outcome unknown.
func isAmbiguous(err error) bool {
	de, ok := rpcx.AsError(err)
	if !ok {
		return true
	}
	switch de.Code {
	case rpcx.CodeUnavailable, rpcx.CodeDeadlineExceeded, rpcx.CodeInternal:
		return true
	default:
		return false
	}
}

// startRejection maps a pre-start failure to (receipt status, reason).
func startRejection(err error) (string, string) {
	de, ok := rpcx.AsError(err)
	if !ok {
		return model.ScheduledReceiptError, model.ScheduledReasonStartRejected
	}
	switch de.Code {
	case rpcx.CodeSessionBusy:
		return model.ScheduledReceiptSkipped, model.ScheduledReasonSessionBusy
	case rpcx.CodeUserUnavailable:
		return model.ScheduledReceiptError, model.ScheduledReasonUserUnavailable
	case rpcx.CodeSessionPolicy:
		return model.ScheduledReceiptSkipped, model.ScheduledSessionPolicyMigrationReason
	default:
		return model.ScheduledReceiptError, model.ScheduledReasonStartRejected
	}
}

// terminalFact renders the turn's terminal result as the receipt's terminal
// status and reason. A failed final-content write never claims success.
func terminalFact(res turn.TerminalResult, timedOut bool) (string, string) {
	if !res.MessagesDurable {
		return model.ScheduledReceiptError, model.ScheduledReasonPersistFailed
	}
	if timedOut {
		return model.ScheduledReceiptCancelled, model.ScheduledReasonTaskTimeout
	}
	switch res.Status {
	case run.StatusDone:
		return model.ScheduledReceiptSucceeded, ""
	case run.StatusCancelled:
		return model.ScheduledReceiptCancelled, ""
	case run.StatusInterrupted:
		return model.ScheduledReceiptInterrupted, model.ScheduledReasonResultUnknown
	default:
		return model.ScheduledReceiptError, ""
	}
}

func turnSelection(sel modelselect.Selection) turn.Selection {
	return turn.Selection{
		Model:               sel.Model,
		Thinking:            sel.Thinking,
		Vision:              sel.Vision,
		Effort:              sel.Effort,
		MaxContextTokens:    sel.MaxContextTokens,
		MaxCompletionTokens: sel.MaxCompletionTokens,
		LengthContinue:      sel.LengthContinue,
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func detachedCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
