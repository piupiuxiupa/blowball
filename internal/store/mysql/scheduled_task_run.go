package mysql

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/lush/blowball/internal/model"
)

func (s *Store) ListClaimableScheduledRuns(ctx context.Context, now time.Time, limit int) ([]model.ScheduledTaskRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	now = scheduledUTC(now)
	query := `SELECT ` + scheduledRunColumns + ` FROM scheduled_task_runs
		WHERE (status = 'pending' AND (next_attempt_at IS NULL OR next_attempt_at <= ?))
		   OR (status = 'preparing' AND lease_until IS NOT NULL AND lease_until <= ?)
		ORDER BY scheduled_for ASC, id ASC LIMIT ?`
	logQuery(ctx, "scheduled_task.list_claimable", query, now, limit)
	var runs []model.ScheduledTaskRun
	if err := s.db.SelectContext(ctx, &runs, query, now, now, limit); err != nil {
		return nil, err
	}
	for i := range runs {
		hydrateScheduledRun(&runs[i])
	}
	return runs, nil
}

func (s *Store) ListScheduledRunsForRecovery(ctx context.Context, limit int) ([]model.ScheduledTaskRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	query := `SELECT ` + scheduledRunColumns + ` FROM scheduled_task_runs
		WHERE status IN ('launching', 'running') ORDER BY update_time ASC, id ASC LIMIT ?`
	logQuery(ctx, "scheduled_task.list_recovery", query, limit)
	var runs []model.ScheduledTaskRun
	if err := s.db.SelectContext(ctx, &runs, query, limit); err != nil {
		return nil, err
	}
	for i := range runs {
		hydrateScheduledRun(&runs[i])
	}
	return runs, nil
}

// ClaimScheduledRun leases a safe-not-started occurrence and increments its
// persistent attempt count. launching/running occurrences are never reclaimed.
func (s *Store) ClaimScheduledRun(ctx context.Context, occurrenceID string, now time.Time, lease time.Duration, maxAttempts int) (*model.ScheduledTaskRun, bool, error) {
	now = scheduledUTC(now)
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	tokenID, err := uuid.NewV7()
	if err != nil {
		return nil, false, err
	}
	token := tokenID.String()
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var current struct {
		Status        string     `db:"status"`
		AttemptCount  int        `db:"attempt_count"`
		LeaseUntil    *time.Time `db:"lease_until"`
		NextAttemptAt *time.Time `db:"next_attempt_at"`
	}
	if err := tx.GetContext(ctx, &current, `SELECT status, attempt_count, lease_until, next_attempt_at
		FROM scheduled_task_runs WHERE occurrence_id = ? FOR UPDATE`, occurrenceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	claimable := current.AttemptCount < maxAttempts
	switch current.Status {
	case model.ScheduledRunPending:
		claimable = claimable && (current.NextAttemptAt == nil || !current.NextAttemptAt.After(now))
	case model.ScheduledRunPreparing:
		claimable = claimable && current.LeaseUntil != nil && !current.LeaseUntil.After(now)
	default:
		claimable = false
	}
	if !claimable {
		return nil, false, nil
	}
	leaseUntil := scheduledUTC(now.Add(lease))
	const update = `UPDATE scheduled_task_runs SET status = 'preparing', claim_token = ?, lease_until = ?,
		attempt_count = attempt_count + 1, next_attempt_at = NULL, reason = '', last_error = NULL, update_time = NOW()
		WHERE occurrence_id = ?`
	if _, err := tx.ExecContext(ctx, update, token, leaseUntil, occurrenceID); err != nil {
		return nil, false, err
	}
	var run model.ScheduledTaskRun
	if err := tx.GetContext(ctx, &run, `SELECT `+scheduledRunColumns+` FROM scheduled_task_runs WHERE occurrence_id = ?`, occurrenceID); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	hydrateScheduledRun(&run)
	return &run, true, nil
}

const scheduledFenceAuthorization = ` AND t.enabled = 1 AND t.deleted_at IS NULL AND u.status = 'active'
	AND (r.session_mode = 'new' OR EXISTS (
		SELECT 1 FROM sessions target WHERE target.session_id = r.session_id AND target.user_id = r.user_id
	))`

// MarkScheduledRunLaunching records entry into the possible-execution window.
func (s *Store) MarkScheduledRunLaunching(ctx context.Context, occurrenceID, claimToken string, now time.Time) (bool, error) {
	query := `UPDATE scheduled_task_runs r
		JOIN scheduled_tasks t ON t.task_id = r.task_id
		JOIN users u ON u.user_id = r.user_id
		SET r.status = 'launching', r.update_time = NOW()
		WHERE r.occurrence_id = ? AND r.claim_token = ? AND r.status = 'preparing'
		  AND r.started_at IS NULL AND r.lease_until IS NOT NULL AND r.lease_until > ?` + scheduledFenceAuthorization
	logQuery(ctx, "scheduled_task.mark_launching", query, occurrenceID)
	res, err := s.db.ExecContext(ctx, query, occurrenceID, claimToken, scheduledUTC(now))
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected == 1, err
}

// CrossScheduledStartFence atomically converts a valid launching claim into a
// running occurrence. started_at is the timeout clock's origin and the
// pause/delete boundary: only rows with started_at NULL can be cancelled.
func (s *Store) CrossScheduledStartFence(ctx context.Context, occurrenceID, claimToken string, now time.Time) (bool, error) {
	query := `UPDATE scheduled_task_runs r
		JOIN scheduled_tasks t ON t.task_id = r.task_id
		JOIN users u ON u.user_id = r.user_id
		SET r.status = 'running', r.started_at = ?, r.lease_until = NULL, r.next_attempt_at = NULL,
		    r.reason = '', r.last_error = NULL, r.update_time = NOW()
		WHERE r.occurrence_id = ? AND r.claim_token = ? AND r.status = 'launching'
		  AND r.started_at IS NULL AND r.lease_until IS NOT NULL AND r.lease_until > ?` + scheduledFenceAuthorization
	logQuery(ctx, "scheduled_task.cross_start_fence", query, occurrenceID)
	now = scheduledUTC(now)
	res, err := s.db.ExecContext(ctx, query, now, occurrenceID, claimToken, now)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected == 1, err
}

// ScheduleScheduledRunRetry returns a definitely-unstarted claim to pending
// with durable backoff. An expired or superseded token cannot move the row.
func (s *Store) ScheduleScheduledRunRetry(ctx context.Context, occurrenceID, claimToken string, nextAttempt time.Time, lastError string) (bool, error) {
	const query = `UPDATE scheduled_task_runs SET status = 'pending', claim_token = '', lease_until = NULL,
		next_attempt_at = ?, last_error = NULLIF(?, ''), update_time = NOW()
		WHERE occurrence_id = ? AND claim_token = ? AND started_at IS NULL AND status IN ('preparing', 'launching')`
	res, err := s.db.ExecContext(ctx, query, scheduledUTC(nextAttempt), lastError, occurrenceID, claimToken)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected == 1, err
}

// FinishScheduledRunBeforeStart records a terminal validation/admission result
// for the current lease holder, before any model execution may have begun.
func (s *Store) FinishScheduledRunBeforeStart(ctx context.Context, occurrenceID, claimToken, status, reason, lastError string, now time.Time) (bool, error) {
	const query = `UPDATE scheduled_task_runs SET status = ?, reason = ?, last_error = NULLIF(?, ''),
		finished_at = ?, claim_token = '', lease_until = NULL, next_attempt_at = NULL, update_time = NOW()
		WHERE occurrence_id = ? AND claim_token = ? AND started_at IS NULL AND status IN ('preparing', 'launching')`
	res, err := s.db.ExecContext(ctx, query, status, reason, lastError, scheduledUTC(now), occurrenceID, claimToken)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected == 1, err
}

// FinishScheduledRun writes a terminal result only while the occurrence is
// still running. A recovered/interrupted row cannot be overwritten by a late
// completion callback from an older executor.
func (s *Store) FinishScheduledRun(ctx context.Context, occurrenceID, status, reason, lastError string, now time.Time) (bool, error) {
	const query = `UPDATE scheduled_task_runs SET status = ?, reason = ?, last_error = NULLIF(?, ''),
		finished_at = ?, claim_token = '', lease_until = NULL, next_attempt_at = NULL, update_time = NOW()
		WHERE occurrence_id = ? AND status IN ('launching', 'running')`
	res, err := s.db.ExecContext(ctx, query, status, reason, lastError, scheduledUTC(now), occurrenceID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected == 1, err
}

// ScheduledRunAuthorization returns an empty string when execution is allowed,
// or the stable pause/skip reason that must be recorded instead.
func (s *Store) ScheduledRunAuthorization(ctx context.Context, occurrenceID string) (string, error) {
	run, err := s.GetScheduledTaskRun(ctx, occurrenceID)
	if err != nil || run == nil {
		return model.ScheduledReasonStartRejected, err
	}
	task, err := s.getScheduledTaskInternal(ctx, run.TaskID)
	if err != nil {
		return "", err
	}
	if task == nil || task.DeletedAt != nil {
		return model.ScheduledReasonTaskDeleted, nil
	}
	if !task.Enabled {
		if task.PausedReason != "" {
			return task.PausedReason, nil
		}
		return model.ScheduledReasonTaskPaused, nil
	}
	var status string
	if err := s.db.GetContext(ctx, &status, `SELECT status FROM users WHERE user_id = ?`, run.UserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.ScheduledReasonUserUnavailable, nil
		}
		return "", err
	}
	if status != model.UserStatusActive {
		return model.ScheduledReasonUserUnavailable, nil
	}
	if run.SessionMode == model.ScheduledSessionExisting {
		var owner string
		if err := s.db.GetContext(ctx, &owner, `SELECT user_id FROM sessions WHERE session_id = ?`, run.SessionID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return model.ScheduledReasonSessionUnavailable, nil
			}
			return "", err
		}
		if owner != run.UserID {
			return model.ScheduledReasonSessionUnavailable, nil
		}
	}
	return "", nil
}

// PauseScheduledTask disables future materialization and cancels all
// not-yet-started occurrences with the supplied stable reason.
func (s *Store) PauseScheduledTask(ctx context.Context, taskID, reason string, now time.Time) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE scheduled_tasks SET enabled = 0, paused_reason = ?,
		revision = revision + 1, update_time = NOW() WHERE task_id = ? AND deleted_at IS NULL`, reason, taskID); err != nil {
		return err
	}
	if err := cancelUnstartedScheduledRuns(ctx, tx, taskID, reason, now); err != nil {
		return err
	}
	return tx.Commit()
}
