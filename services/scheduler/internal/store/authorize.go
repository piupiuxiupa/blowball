package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lush/blowball/pkg/model"
)

var (
	// ErrOccurrenceNotFound reports an unknown occurrence.
	ErrOccurrenceNotFound = errors.New("scheduled occurrence not found")
	// ErrTaskUnavailable reports a paused, deleted or held task. The Runtime
	// must not start.
	ErrTaskUnavailable = errors.New("scheduled task is not startable")
	// ErrGrantFenced reports a stale/replaced generation or a non-launching
	// occurrence.
	ErrGrantFenced = errors.New("scheduled start grant is fenced")
	// ErrGrantNotFound reports no grant for the requested identity.
	ErrGrantNotFound = errors.New("scheduled start grant not found")
)

// AuthorizeStart atomically validates the task/claim/attempt and persists the
// unique start grant. Lock order is task then occurrence, matching the
// pause/delete path, so a pause committed first always wins the startup race.
func (s *Store) AuthorizeStart(ctx context.Context, occurrenceID, runID, sessionID string, attempt int, receiptGeneration uint64, workerGeneration string, now time.Time, duration time.Duration) (*model.ScheduledStartGrant, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var taskID string
	if err := tx.GetContext(ctx, &taskID, `SELECT task_id FROM scheduled_task_runs WHERE occurrence_id = ?`, occurrenceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOccurrenceNotFound
		}
		return nil, err
	}

	// Task first (same order as pause/delete).
	var task struct {
		Enabled   bool           `db:"enabled"`
		DeletedAt sql.NullTime   `db:"deleted_at"`
		Hold      sql.NullString `db:"execution_hold_occurrence_id"`
		Blocked   string         `db:"blocked_reason"`
	}
	if err := tx.GetContext(ctx, &task, `SELECT enabled, deleted_at, execution_hold_occurrence_id, blocked_reason
		FROM scheduled_tasks WHERE task_id = ? FOR UPDATE`, taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTaskUnavailable
		}
		return nil, err
	}
	if !task.Enabled || task.DeletedAt.Valid {
		return nil, ErrTaskUnavailable
	}
	if task.Hold.Valid && task.Hold.String != "" && task.Hold.String != occurrenceID {
		return nil, ErrTaskUnavailable
	}

	var run model.ScheduledTaskRun
	if err := tx.GetContext(ctx, &run, `SELECT `+scheduledRunColumns+`
		FROM scheduled_task_runs WHERE occurrence_id = ? FOR UPDATE`, occurrenceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOccurrenceNotFound
		}
		return nil, err
	}
	if run.RunID != runID || run.SessionID != sessionID || run.AttemptCount != attempt {
		return nil, ErrGrantFenced
	}
	if run.GrantGeneration != 0 {
		if run.GrantGeneration == receiptGeneration && run.GrantDeadline != nil && run.GrantStartedAt != nil {
			// Idempotent retry: return the SAME grant.
			return &model.ScheduledStartGrant{
				OccurrenceID: run.OccurrenceID, Generation: run.GrantGeneration,
				RunID: run.RunID, SessionID: run.SessionID,
				StartedAt: *run.GrantStartedAt, Deadline: *run.GrantDeadline,
			}, nil
		}
		return nil, ErrGrantFenced
	}
	if task.Hold.Valid && task.Hold.String == occurrenceID {
		// This occurrence already owns an execution hold; a new grant could
		// only replay an unproven execution.
		return nil, ErrTaskUnavailable
	}
	if run.Status != model.ScheduledRunLaunching && run.Status != model.ScheduledRunRunning {
		return nil, ErrGrantFenced
	}
	if receiptGeneration == 0 {
		return nil, ErrGrantFenced
	}

	startedAt := utc(now)
	deadline := utc(now.Add(duration))
	if _, err := tx.ExecContext(ctx, `UPDATE scheduled_task_runs
SET status = 'running', started_at = COALESCE(started_at, ?), receipt_generation = ?,
    grant_generation = ?, grant_started_at = ?, grant_deadline = ?,
    runtime_worker_generation = ?, claim_token = '', lease_until = NULL, update_time = NOW()
WHERE occurrence_id = ?`, startedAt, receiptGeneration, receiptGeneration, startedAt, deadline,
		workerGeneration, occurrenceID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &model.ScheduledStartGrant{
		OccurrenceID: run.OccurrenceID, Generation: receiptGeneration,
		RunID: run.RunID, SessionID: run.SessionID,
		StartedAt: startedAt, Deadline: deadline,
	}, nil
}

// GetStartGrant returns the persisted grant for the same generation, or
// ErrGrantNotFound.
func (s *Store) GetStartGrant(ctx context.Context, occurrenceID string, receiptGeneration uint64) (*model.ScheduledStartGrant, error) {
	run, err := s.GetOccurrence(ctx, occurrenceID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrOccurrenceNotFound
	}
	if run.GrantGeneration != receiptGeneration || run.GrantStartedAt == nil || run.GrantDeadline == nil {
		return nil, ErrGrantNotFound
	}
	return &model.ScheduledStartGrant{
		OccurrenceID: run.OccurrenceID, Generation: run.GrantGeneration,
		RunID: run.RunID, SessionID: run.SessionID,
		StartedAt: *run.GrantStartedAt, Deadline: *run.GrantDeadline,
	}, nil
}
