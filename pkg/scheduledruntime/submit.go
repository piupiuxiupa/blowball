package scheduledruntime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/modelselect"
	"github.com/lush/blowball/pkg/rpcx"
	"github.com/lush/blowball/pkg/scheduledcontract"
	"github.com/lush/blowball/pkg/store/mysql"
)

// SubmitInput is the neutral form of SubmitScheduledRun.
type SubmitInput struct {
	OccurrenceID      string
	TaskID            string
	RunID             string
	SessionID         string
	Attempt           int
	ConfigFingerprint string
}

// SubmitResult reports the durable acceptance.
type SubmitResult struct {
	Receipt         *model.ScheduledExecutionReceipt
	AlreadyAccepted bool
}

// Submit performs the idempotent acceptance transaction: the Runtime fetches
// the authoritative occurrence, re-validates owner/model/resources and the
// configuration fingerprint, then transactionally creates the scheduled
// session and the execution receipt. A missing response can be retried with
// the same identity and resolves to the same rows.
func (r *Runtime) Submit(ctx context.Context, in SubmitInput) (*SubmitResult, error) {
	if in.OccurrenceID == "" || in.TaskID == "" || in.RunID == "" || in.SessionID == "" {
		return nil, rpcx.Errorf(rpcx.CodeInvalidArgument, "occurrence_id, task_id, run_id and session_id are required")
	}
	occ, _, _, _, err := r.loadAuthoritative(ctx, in.OccurrenceID)
	if err != nil {
		return nil, err
	}
	if occ.TaskID != in.TaskID || occ.RunID != in.RunID || occ.SessionID != in.SessionID {
		return nil, rpcx.Errorf(rpcx.CodeConflict, "submission identity does not match the authoritative occurrence")
	}
	fingerprint := scheduledcontract.Fingerprint(scheduledcontract.FingerprintInput{
		TaskID:           occ.TaskID,
		OccurrenceID:     occ.OccurrenceID,
		RunID:            occ.RunID,
		SessionID:        occ.SessionID,
		SnapshotJSON:     occ.SnapshotJSON,
		ResourceRevision: occ.ResourceRevision,
		ManifestHash:     occ.ManifestHash,
	})
	if fingerprint != in.ConfigFingerprint {
		return nil, rpcx.Errorf(rpcx.CodeConflict, "configuration fingerprint does not match the authoritative occurrence")
	}

	now := r.now().UTC()
	receipt := model.ScheduledExecutionReceipt{
		OccurrenceID:      occ.OccurrenceID,
		TaskID:            occ.TaskID,
		UserID:            occ.UserID,
		RunID:             occ.RunID,
		SessionID:         occ.SessionID,
		AttemptCount:      in.Attempt,
		ConfigFingerprint: fingerprint,
		ReceiptGeneration: 1,
		Status:            model.ScheduledReceiptAccepted,
		AcceptedAt:        now,
	}
	session := model.Session{
		SessionID:    occ.SessionID,
		UserID:       occ.UserID,
		TaskID:       occ.TaskID,
		OccurrenceID: occ.OccurrenceID,
		RunID:        occ.RunID,
		TraceID:      occ.RunID,
		Source:       model.SessionSourceScheduled,
	}
	stored, already, err := r.store.AcceptScheduledExecution(ctx, receipt, session)
	if err != nil {
		return nil, mapAcceptanceError(err)
	}
	return &SubmitResult{Receipt: stored, AlreadyAccepted: already}, nil
}

// GetExecution returns the authoritative receipt, or a NOT_FOUND domain error.
func (r *Runtime) GetExecution(ctx context.Context, occurrenceID string) (*model.ScheduledExecutionReceipt, error) {
	if occurrenceID == "" {
		return nil, rpcx.Errorf(rpcx.CodeInvalidArgument, "occurrence_id is required")
	}
	rec, err := r.store.GetScheduledExecution(ctx, occurrenceID)
	if err != nil {
		return nil, rpcx.Wrap(rpcx.CodeUnavailable, "execution receipt read failed", err)
	}
	if rec == nil {
		return nil, rpcx.Errorf(rpcx.CodeNotFound, "scheduled execution %q not found", occurrenceID)
	}
	return rec, nil
}

// CancelExecution requests cancellation. A pre-start execution is terminalized
// immediately (no side effect can have occurred); a started execution is
// cancelled through the run registry/flag. A successful acceptance here is NOT
// a stop proof.
func (r *Runtime) CancelExecution(ctx context.Context, occurrenceID, reason string) (bool, string, error) {
	rec, err := r.store.GetScheduledExecution(ctx, occurrenceID)
	if err != nil {
		return false, "", rpcx.Wrap(rpcx.CodeUnavailable, "execution receipt read failed", err)
	}
	if rec == nil {
		return false, "", rpcx.Errorf(rpcx.CodeNotFound, "scheduled execution %q not found", occurrenceID)
	}
	if rec.Terminal() {
		return false, rec.Status, nil
	}
	if reason == "" {
		reason = model.ScheduledReasonCancelledByUser
	}
	if rec.Status == model.ScheduledReceiptAccepted || rec.Status == model.ScheduledReceiptPreparing {
		ok, cerr := r.store.CancelScheduledReceiptBeforeStart(ctx, rec.OccurrenceID, rec.ReceiptGeneration, reason, "", r.now())
		if cerr != nil {
			return false, rec.Status, rpcx.Wrap(rpcx.CodeUnavailable, "cancel persist failed", cerr)
		}
		if ok {
			return true, model.ScheduledReceiptCancelled, nil
		}
		// A worker won the start race; fall through with the fresh state.
		rec, err = r.store.GetScheduledExecution(ctx, occurrenceID)
		if err != nil || rec == nil {
			return false, "", rpcx.Wrap(rpcx.CodeUnavailable, "execution receipt re-read failed", err)
		}
		if rec.Terminal() {
			return false, rec.Status, nil
		}
	}
	if err := r.canceller.RequestCancel(ctx, rec.RunID); err != nil {
		return false, rec.Status, rpcx.Wrap(rpcx.CodeUnavailable, "cancel request failed", err)
	}
	return true, rec.Status, nil
}

// loadAuthoritative fetches and validates the authoritative occurrence. It is
// the single validation point for acceptance and execution; neither trusts the
// submission body. The returned resource-context string is the bounded
// manifest pointer injected into the turn (empty when no resources).
func (r *Runtime) loadAuthoritative(ctx context.Context, occurrenceID string) (*Occurrence, model.ScheduledTaskSnapshot, modelselect.Selection, string, error) {
	occ, err := r.occurrences.GetOccurrence(ctx, occurrenceID)
	if err != nil {
		if _, ok := rpcx.AsError(err); ok {
			return nil, model.ScheduledTaskSnapshot{}, modelselect.Selection{}, "", err
		}
		return nil, model.ScheduledTaskSnapshot{}, modelselect.Selection{}, "",
			rpcx.Wrap(rpcx.CodeUnavailable, "authoritative occurrence unavailable", err)
	}
	if occ == nil {
		return nil, model.ScheduledTaskSnapshot{}, modelselect.Selection{}, "",
			rpcx.Errorf(rpcx.CodeNotFound, "scheduled occurrence %q not found", occurrenceID)
	}
	if occ.SessionSource != string(model.SessionSourceScheduled) {
		return nil, model.ScheduledTaskSnapshot{}, modelselect.Selection{}, "",
			rpcx.Errorf(rpcx.CodeSessionPolicy,
				"occurrence %q belongs to legacy_interactive history; per-occurrence scheduled sessions are required", occurrenceID)
	}
	var snap model.ScheduledTaskSnapshot
	if err := json.Unmarshal([]byte(occ.SnapshotJSON), &snap); err != nil {
		return nil, model.ScheduledTaskSnapshot{}, modelselect.Selection{}, "",
			rpcx.Wrap(rpcx.CodeInternal, "authoritative snapshot is invalid", err)
	}
	if snap.TaskID != occ.TaskID || snap.UserID != occ.UserID {
		return nil, model.ScheduledTaskSnapshot{}, modelselect.Selection{}, "",
			rpcx.Errorf(rpcx.CodeConflict, "occurrence snapshot identity mismatch")
	}

	user, err := r.users.GetUserByID(ctx, occ.UserID)
	if err != nil {
		return nil, model.ScheduledTaskSnapshot{}, modelselect.Selection{}, "",
			rpcx.Wrap(rpcx.CodeUnavailable, "owner lookup failed", err)
	}
	if user == nil || user.Status != model.UserStatusActive {
		return nil, model.ScheduledTaskSnapshot{}, modelselect.Selection{}, "",
			rpcx.Errorf(rpcx.CodeUserUnavailable, "task owner is not available")
	}

	sel, code, message := modelselect.Resolve(r.selection, snap.Model, snap.ReasoningEffort)
	if code != "" {
		return nil, model.ScheduledTaskSnapshot{}, modelselect.Selection{}, "", rpcx.Errorf(code, "%s", message)
	}

	resourceContext := ""
	if occ.ResourceRevision > 0 {
		if r.resources == nil {
			return nil, model.ScheduledTaskSnapshot{}, modelselect.Selection{}, "",
				rpcx.Errorf(rpcx.CodeUnavailable, "pinned resource validation is not configured")
		}
		ctxText, rerr := r.resources.ValidatePinned(ctx, occ.UserID, occ.TaskID, occ.ResourceRevision, occ.ManifestHash)
		if rerr != nil {
			return nil, model.ScheduledTaskSnapshot{}, modelselect.Selection{}, "",
				rpcx.Wrap(rpcx.CodeFailedPrecondition, "pinned resource validation failed", rerr)
		}
		resourceContext = ctxText
	}
	return occ, snap, sel, resourceContext, nil
}

func mapAcceptanceError(err error) error {
	switch {
	case errors.Is(err, mysql.ErrScheduledReceiptConflict), errors.Is(err, mysql.ErrScheduledSessionConflict):
		return rpcx.Wrap(rpcx.CodeConflict, "acceptance identity conflict", err)
	default:
		return rpcx.Wrap(rpcx.CodeUnavailable, "acceptance persist failed", err)
	}
}

// fingerprintOf recomputes the configuration identity from the authoritative
// occurrence.
func fingerprintOf(occ *Occurrence) string {
	return scheduledcontract.Fingerprint(scheduledcontract.FingerprintInput{
		TaskID:           occ.TaskID,
		OccurrenceID:     occ.OccurrenceID,
		RunID:            occ.RunID,
		SessionID:        occ.SessionID,
		SnapshotJSON:     occ.SnapshotJSON,
		ResourceRevision: occ.ResourceRevision,
		ManifestHash:     occ.ManifestHash,
	})
}
