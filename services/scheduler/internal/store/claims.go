package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/lush/blowball/pkg/model"
)

const terminalStatusPredicate = `status NOT IN ('succeeded','error','skipped','cancelled','interrupted')`

// ListClaimableRuns returns due pending runs and stale preparations. Callers
// then CAS-claim one at a time.
func (s *Store) ListClaimableRuns(ctx context.Context, now time.Time, limit, maxAttempts int) ([]model.ScheduledTaskRun, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT ` + scheduledRunColumns + ` FROM scheduled_task_runs
		WHERE attempt_count < ?
		  AND ((status = 'pending' AND (next_attempt_at IS NULL OR next_attempt_at <= ?))
		    OR (status = 'preparing' AND (lease_until IS NULL OR lease_until < ?)))
		ORDER BY scheduled_for ASC, id ASC LIMIT ?`
	var runs []model.ScheduledTaskRun
	if err := s.db.SelectContext(ctx, &runs, query, maxAttempts, utc(now), utc(now), limit); err != nil {
		return nil, err
	}
	for i := range runs {
		hydrateRun(&runs[i])
	}
	return runs, nil
}

// ClaimRun CAS-claims one occurrence for preparation. The attempt counter is
// the persistent bounded-retry budget.
func (s *Store) ClaimRun(ctx context.Context, occurrenceID string, now time.Time, lease time.Duration, maxAttempts int) (*model.ScheduledTaskRun, bool, error) {
	token := uuid.NewString()
	query := `UPDATE scheduled_task_runs
SET status = 'preparing', claim_token = ?, lease_until = ?, attempt_count = attempt_count + 1,
    next_attempt_at = NULL, update_time = NOW()
WHERE occurrence_id = ? AND attempt_count < ?
  AND ((status = 'pending' AND (next_attempt_at IS NULL OR next_attempt_at <= ?))
    OR (status = 'preparing' AND (lease_until IS NULL OR lease_until < ?)))`
	res, err := s.db.ExecContext(ctx, query, token, utc(now.Add(lease)), occurrenceID, maxAttempts, utc(now), utc(now))
	if err != nil {
		return nil, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	if n != 1 {
		return nil, false, nil
	}
	run, err := s.GetOccurrence(ctx, occurrenceID)
	if err != nil || run == nil {
		return nil, false, err
	}
	return run, true, nil
}

// MarkLaunching records that the Runtime submission is starting (or was
// attempted) for this claim.
func (s *Store) MarkLaunching(ctx context.Context, occurrenceID, claimToken string, now time.Time, lease time.Duration) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_task_runs
SET status = 'launching', lease_until = ?, update_time = NOW()
WHERE occurrence_id = ? AND claim_token = ? AND status = 'preparing'`,
		utc(now.Add(lease)), occurrenceID, claimToken)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetReceiptGeneration records the Runtime receipt generation the occurrence
// corresponds to (learned from acceptance or reconciliation).
func (s *Store) SetReceiptGeneration(ctx context.Context, occurrenceID string, generation uint64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scheduled_task_runs SET receipt_generation = ?, update_time = NOW()
WHERE occurrence_id = ? AND `+terminalStatusPredicate+` AND receipt_generation < ?`,
		generation, occurrenceID, generation)
	return err
}

// ScheduleRetry returns the occurrence to pending with a bounded backoff. The
// attempt counter was already incremented by ClaimRun.
func (s *Store) ScheduleRetry(ctx context.Context, occurrenceID, claimToken, reason, lastError string, next time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_task_runs
SET status = 'pending', reason = ?, last_error = NULLIF(?, ''), claim_token = '', lease_until = NULL,
    next_attempt_at = ?, update_time = NOW()
WHERE occurrence_id = ? AND claim_token = ? AND status IN ('preparing','launching')`,
		reason, lastError, utc(next), occurrenceID, claimToken)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RequeueRun returns a launching/preparing occurrence to pending without a
// claim-token match (reconciliation path). The attempt counter is unchanged.
func (s *Store) RequeueRun(ctx context.Context, occurrenceID, reason, lastError string, next time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_task_runs
SET status = 'pending', reason = ?, last_error = NULLIF(?, ''), claim_token = '', lease_until = NULL,
    next_attempt_at = ?, update_time = NOW()
WHERE occurrence_id = ? AND grant_generation = 0 AND status IN ('pending','preparing','launching')`,
		reason, lastError, utc(next), occurrenceID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ListExhaustedRuns returns occurrences whose bounded preparation attempts are
// used up without ever crossing a grant.
func (s *Store) ListExhaustedRuns(ctx context.Context, maxAttempts, limit int) ([]model.ScheduledTaskRun, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT ` + scheduledRunColumns + ` FROM scheduled_task_runs
		WHERE attempt_count >= ? AND grant_generation = 0
		  AND status IN ('pending','preparing','launching')
		  AND (lease_until IS NULL OR lease_until < NOW(3))
		ORDER BY scheduled_for ASC, id ASC LIMIT ?`
	var runs []model.ScheduledTaskRun
	if err := s.db.SelectContext(ctx, &runs, query, maxAttempts, limit); err != nil {
		return nil, err
	}
	for i := range runs {
		hydrateRun(&runs[i])
	}
	return runs, nil
}

// ListHeldOccurrences returns the occurrences currently owning their task's
// execution hold.
func (s *Store) ListHeldOccurrences(ctx context.Context, limit int) ([]model.ScheduledTaskRun, error) {
	if limit <= 0 {
		limit = 100
	}
	// The IN-subselect avoids ambiguous column names in the JOIN's projection
	// while keeping the outer scan on the canonical column list.
	query := `SELECT ` + scheduledRunColumns + ` FROM scheduled_task_runs
		WHERE occurrence_id IN (
			SELECT r.occurrence_id FROM scheduled_task_runs r
			JOIN scheduled_tasks t ON t.task_id = r.task_id
			  AND t.execution_hold_occurrence_id = r.occurrence_id
		)
		ORDER BY scheduled_for ASC, id ASC LIMIT ?`
	var runs []model.ScheduledTaskRun
	if err := s.db.SelectContext(ctx, &runs, query, limit); err != nil {
		return nil, err
	}
	for i := range runs {
		hydrateRun(&runs[i])
	}
	return runs, nil
}

// FinishExhausted terminalizes a run whose bounded preparation attempts are
// used up. Unlike FinishBeforeStart it does not require a claim token (the
// final retry returned the run to pending).
func (s *Store) FinishExhausted(ctx context.Context, occurrenceID, status, reason, lastError string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_task_runs
SET status = ?, reason = ?, last_error = NULLIF(?, ''), finished_at = ?, claim_token = '',
    lease_until = NULL, next_attempt_at = NULL, update_time = NOW()
WHERE occurrence_id = ? AND grant_generation = 0 AND status IN ('pending','preparing','launching')`,
		status, reason, lastError, utc(now), occurrenceID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FinishBeforeStart writes a pre-start terminal projection for the current
// claim holder.
func (s *Store) FinishBeforeStart(ctx context.Context, occurrenceID, claimToken, status, reason, lastError string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_task_runs
SET status = ?, reason = ?, last_error = NULLIF(?, ''), claim_token = '', lease_until = NULL,
    next_attempt_at = NULL, finished_at = ?, update_time = NOW()
WHERE occurrence_id = ? AND claim_token = ? AND grant_generation = 0
  AND status IN ('preparing','launching')`,
		status, reason, lastError, utc(now), occurrenceID, claimToken)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FinishOccurrence projects a verified Runtime terminal fact. A stale/late
// projection cannot overwrite an already-terminal occurrence.
func (s *Store) FinishOccurrence(ctx context.Context, occurrenceID, status, reason, lastError string, receiptGeneration uint64, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_task_runs
SET status = ?, reason = ?, last_error = NULLIF(?, ''), claim_token = '', lease_until = NULL,
    next_attempt_at = NULL, finished_at = ?, terminal_receipt_generation = ?, update_time = NOW()
WHERE occurrence_id = ? AND `+terminalStatusPredicate,
		status, reason, lastError, utc(now), receiptGeneration, occurrenceID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		// Idempotent when the same terminal fact is already projected.
		run, getErr := s.GetOccurrence(ctx, occurrenceID)
		return run != nil && model.ScheduledRunTerminal(run.Status) && run.TerminalReceiptGeneration == receiptGeneration, getErr
	}
	return n == 1, err
}

// MarkRunningFromReceipt records the authoritative start and generation.
func (s *Store) MarkRunningFromReceipt(ctx context.Context, occurrenceID string, receiptGeneration uint64, startedAt *time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scheduled_task_runs
SET status = 'running', receipt_generation = ?, started_at = COALESCE(started_at, ?),
    claim_token = '', lease_until = NULL, update_time = NOW()
WHERE occurrence_id = ? AND `+terminalStatusPredicate,
		receiptGeneration, startedAt, occurrenceID)
	return err
}

// MarkInterruptedFromReceipt projects an uncertain/unproven execution as
// interrupted/RESULT_UNKNOWN. It never claims the execution stopped.
func (s *Store) MarkInterruptedFromReceipt(ctx context.Context, occurrenceID string, receiptGeneration uint64, lastError string, now time.Time) (bool, error) {
	return s.FinishOccurrence(ctx, occurrenceID, model.ScheduledRunInterrupted,
		model.ScheduledReasonResultUnknown, lastError, receiptGeneration, now)
}

// ListRecoverableRuns returns non-terminal occurrences that may have a Runtime
// execution fact to reconcile.
func (s *Store) ListRecoverableRuns(ctx context.Context, limit int) ([]model.ScheduledTaskRun, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT ` + scheduledRunColumns + ` FROM scheduled_task_runs
		WHERE ` + terminalStatusPredicate + `
		  AND (status IN ('launching','running') OR receipt_generation > 0)
		ORDER BY scheduled_for ASC, id ASC LIMIT ?`
	var runs []model.ScheduledTaskRun
	if err := s.db.SelectContext(ctx, &runs, query, limit); err != nil {
		return nil, err
	}
	for i := range runs {
		hydrateRun(&runs[i])
	}
	return runs, nil
}

// SetHold persists the task-level execution hold. The first holding occurrence
// wins; a second occurrence cannot silently replace it.
func (s *Store) SetHold(ctx context.Context, taskID, occurrenceID, reason string, now time.Time) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var current sql.NullString
	if err := tx.GetContext(ctx, &current, `SELECT execution_hold_occurrence_id FROM scheduled_tasks
		WHERE task_id = ? FOR UPDATE`, taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if current.Valid && current.String != "" && current.String != occurrenceID {
		return nil // an earlier unproven execution owns the hold
	}
	if _, err := tx.ExecContext(ctx, `UPDATE scheduled_tasks SET execution_hold_occurrence_id = ?,
		blocked_reason = ?, update_time = NOW() WHERE task_id = ?`, occurrenceID, reason, taskID); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearHold performs the generation-bound CAS clear. Only a positive stop
// proof for the holding occurrence may call it.
func (s *Store) ClearHold(ctx context.Context, taskID, occurrenceID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_tasks
SET execution_hold_occurrence_id = NULL, blocked_reason = '', update_time = NOW()
WHERE task_id = ? AND execution_hold_occurrence_id = ?`, taskID, occurrenceID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CountActiveOccurrences reports the managed occupancy projection: any
// non-terminal occurrence plus any held task counts against capacity.
func (s *Store) CountActiveOccurrences(ctx context.Context) (int, error) {
	var n int
	err := s.db.GetContext(ctx, &n, `SELECT COUNT(*) FROM scheduled_task_runs WHERE `+terminalStatusPredicate)
	return n, err
}
