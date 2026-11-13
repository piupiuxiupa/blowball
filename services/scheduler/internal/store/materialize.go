package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/schedule"
)

// MaterializeTasks atomically claims due tasks, inserts one occurrence per
// selected task, and advances next_run_at. The unique (task_id, scheduled_for)
// slot is the cross-replica backstop; FOR UPDATE SKIP LOCKED serializes the
// normal path.
func (s *Store) MaterializeTasks(ctx context.Context, now time.Time, limit int) ([]model.ScheduledTaskRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	now = utc(now)
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	query := `SELECT task_id FROM scheduled_tasks
		WHERE deleted_at IS NULL AND enabled = 1 AND next_run_at IS NOT NULL AND next_run_at <= ?
		ORDER BY next_run_at ASC, task_id ASC LIMIT ? FOR UPDATE SKIP LOCKED`
	var ids []string
	if err := tx.SelectContext(ctx, &ids, query, now, limit); err != nil {
		return nil, err
	}
	runs := make([]model.ScheduledTaskRun, 0, len(ids))
	for _, taskID := range ids {
		run, err := materializeOne(ctx, tx, taskID, now)
		if err != nil {
			return nil, err
		}
		if run != nil {
			runs = append(runs, *run)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return runs, nil
}

func materializeOne(ctx context.Context, tx *sqlx.Tx, taskID string, now time.Time) (*model.ScheduledTaskRun, error) {
	query := `SELECT ` + scheduledTaskColumns + ` FROM scheduled_tasks WHERE task_id = ?`
	var task model.ScheduledTask
	if err := tx.GetContext(ctx, &task, query, taskID); err != nil {
		return nil, fmt.Errorf("materialize task %s: %w", taskID, err)
	}
	hydrateTask(&task)
	if task.NextRunAt == nil {
		return nil, fmt.Errorf("materialize task %s: next_run_at disappeared under lock", taskID)
	}
	scheduledFor := utc(*task.NextRunAt)

	skipStatus, skipReason := "", ""
	switch {
	case task.SessionMode == model.ScheduledSessionExisting:
		// The retired policy never executes again. The owner must explicitly
		// migrate the task; no ordinary session is touched.
		skipStatus, skipReason = model.ScheduledRunSkipped, model.ScheduledSessionPolicyMigrationReason
	case task.ExecutionHoldOccurrenceID != "":
		// An unproven execution may still be running: every later slot is
		// skipped and the plan advances.
		skipStatus, skipReason = model.ScheduledRunSkipped, model.ScheduledReasonExecutionUncertain
	default:
		active, err := hasActiveOccurrence(ctx, tx, taskID)
		if err != nil {
			return nil, err
		}
		if active {
			skipStatus, skipReason = model.ScheduledRunSkipped, model.ScheduledReasonTaskOverlap
		}
	}

	run, err := newRun(&task, scheduledFor, now, skipStatus, skipReason)
	if err != nil {
		return nil, err
	}
	const insert = `INSERT INTO scheduled_task_runs (
		occurrence_id, task_id, user_id, revision, snapshot_json,
		resource_revision, manifest_hash, scheduled_for, run_id, session_id, session_mode,
		session_source, status, reason, next_attempt_at, finished_at, create_time, update_time
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'scheduled', ?, ?, ?, ?, NOW(), NOW())
	ON DUPLICATE KEY UPDATE occurrence_id = occurrence_id`
	if _, err := tx.ExecContext(ctx, insert,
		run.OccurrenceID, run.TaskID, run.UserID, run.Revision, run.SnapshotJSON,
		run.ResourceRevision, run.ManifestHash, run.ScheduledFor, run.RunID, run.SessionID,
		run.SessionMode, run.Status, run.Reason, run.NextAttemptAt, run.FinishedAt); err != nil {
		return nil, fmt.Errorf("materialize task %s slot %s: %w", taskID, scheduledFor, err)
	}

	var next *time.Time
	if task.ScheduleType == schedule.TypeCron {
		value, err := schedule.Next(task.Rule(), now)
		if err != nil {
			return nil, fmt.Errorf("advance task %s: %w", taskID, err)
		}
		next = &value
	}
	if _, err := tx.ExecContext(ctx, `UPDATE scheduled_tasks SET next_run_at = ?, update_time = NOW()
		WHERE task_id = ?`, next, taskID); err != nil {
		return nil, err
	}
	return run, nil
}

func hasActiveOccurrence(ctx context.Context, tx *sqlx.Tx, taskID string) (bool, error) {
	const query = `SELECT occurrence_id FROM scheduled_task_runs
		WHERE task_id = ? AND status IN ('pending','preparing','launching','running') LIMIT 1`
	var id string
	if err := tx.GetContext(ctx, &id, query, taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func newRun(task *model.ScheduledTask, scheduledFor, now time.Time, skipStatus, skipReason string) (*model.ScheduledTaskRun, error) {
	newID := func() (string, error) {
		id, err := uuid.NewV7()
		if err != nil {
			return "", err
		}
		return id.String(), nil
	}
	occurrenceID, err := newID()
	if err != nil {
		return nil, err
	}
	runID, err := newID()
	if err != nil {
		return nil, err
	}
	sessionID, err := newID()
	if err != nil {
		return nil, err
	}
	snapshot := task.Snapshot()
	// New occurrences always own a fresh per-occurrence session, even when an
	// old task row still carries the retired session.mode=existing field.
	snapshot.Session = model.ScheduledSessionPolicy{Mode: model.ScheduledSessionNew, SessionID: sessionID}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	run := &model.ScheduledTaskRun{
		OccurrenceID: occurrenceID, TaskID: task.TaskID, UserID: task.UserID,
		Revision: task.Revision, SnapshotJSON: string(snapshotJSON),
		ResourceRevision: task.ResourceRevision, ManifestHash: task.ManifestHash,
		ScheduledFor: scheduledFor, RunID: runID, SessionID: sessionID,
		SessionMode: model.ScheduledSessionNew, SessionSource: string(model.SessionSourceScheduled),
		Status: model.ScheduledRunPending, NextAttemptAt: &now, CreateTime: now, UpdateTime: now,
	}
	if skipStatus != "" {
		run.Status = skipStatus
		run.Reason = skipReason
		run.NextAttemptAt = nil
		run.FinishedAt = &now
	}
	return run, nil
}
