package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/schedule"
)

// MaterializeScheduledTasks atomically claims due task rows, inserts one
// occurrence per selected task, and advances next_run_at in the same
// transaction. Cron recovery coalesces all missed slots to the earliest saved
// slot, then schedules strictly after now.
func (s *Store) MaterializeScheduledTasks(ctx context.Context, now time.Time, limit int) ([]model.ScheduledTaskRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	now = scheduledUTC(now)
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	query := `SELECT task_id FROM scheduled_tasks
		WHERE deleted_at IS NULL AND enabled = 1 AND next_run_at IS NOT NULL AND next_run_at <= ?
		ORDER BY next_run_at ASC, task_id ASC LIMIT ? FOR UPDATE SKIP LOCKED`
	logQuery(ctx, "scheduled_task.materialize_scan", query, now, limit)
	var ids []string
	if err := tx.SelectContext(ctx, &ids, query, now, limit); err != nil {
		return nil, err
	}
	runs := make([]model.ScheduledTaskRun, 0, len(ids))
	for _, taskID := range ids {
		run, err := materializeOneScheduledTask(ctx, tx, taskID, now)
		if err != nil {
			return nil, err
		}
		runs = append(runs, *run)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return runs, nil
}

func materializeOneScheduledTask(ctx context.Context, tx *sqlx.Tx, taskID string, now time.Time) (*model.ScheduledTaskRun, error) {
	query := `SELECT ` + scheduledTaskColumns + ` FROM scheduled_tasks WHERE task_id = ?`
	var task model.ScheduledTask
	if err := tx.GetContext(ctx, &task, query, taskID); err != nil {
		return nil, fmt.Errorf("materialize task %s: %w", taskID, err)
	}
	hydrateScheduledTask(&task)
	if task.NextRunAt == nil {
		return nil, fmt.Errorf("materialize task %s: next_run_at disappeared under lock", taskID)
	}
	scheduledFor := scheduledUTC(*task.NextRunAt)

	const activeQuery = `SELECT occurrence_id FROM scheduled_task_runs
		WHERE task_id = ? AND status IN ('pending', 'preparing', 'launching', 'running') LIMIT 1`
	var active string
	overlap := true
	if err := tx.GetContext(ctx, &active, activeQuery, taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			overlap = false
		} else {
			return nil, err
		}
	}

	run, err := newScheduledTaskRun(&task, scheduledFor, now, overlap)
	if err != nil {
		return nil, err
	}
	const insert = `INSERT INTO scheduled_task_runs (
		occurrence_id, task_id, user_id, revision, snapshot_json, scheduled_for, run_id, session_id, session_mode,
		status, reason, next_attempt_at, finished_at, create_time, update_time
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(), NOW())`
	logQuery(ctx, "scheduled_task.materialize_insert", insert, taskID, scheduledFor)
	if _, err := tx.ExecContext(ctx, insert,
		run.OccurrenceID, run.TaskID, run.UserID, run.Revision, run.SnapshotJSON, run.ScheduledFor,
		run.RunID, run.SessionID, run.SessionMode, run.Status, run.Reason, run.NextAttemptAt, run.FinishedAt); err != nil {
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
	const advance = `UPDATE scheduled_tasks SET next_run_at = ?, update_time = NOW() WHERE task_id = ?`
	if _, err := tx.ExecContext(ctx, advance, next, taskID); err != nil {
		return nil, err
	}
	return run, nil
}

func newScheduledTaskRun(task *model.ScheduledTask, scheduledFor, now time.Time, overlap bool) (*model.ScheduledTaskRun, error) {
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
	sessionID := ""
	if task.SessionMode == model.ScheduledSessionExisting && task.SessionID != nil {
		sessionID = *task.SessionID
	} else {
		sessionID, err = newID()
		if err != nil {
			return nil, err
		}
	}
	snapshot := task.Snapshot()
	if snapshot.Session.Mode == model.ScheduledSessionNew {
		snapshot.Session.SessionID = sessionID
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	run := &model.ScheduledTaskRun{
		OccurrenceID: occurrenceID, TaskID: task.TaskID, UserID: task.UserID,
		Revision: task.Revision, SnapshotJSON: string(snapshotJSON), ScheduledFor: scheduledFor,
		RunID: runID, SessionID: sessionID, SessionMode: task.SessionMode, Status: model.ScheduledRunPending,
		NextAttemptAt: &now, CreateTime: now, UpdateTime: now,
	}
	if overlap {
		run.Status = model.ScheduledRunSkipped
		run.Reason = model.ScheduledReasonTaskOverlap
		run.NextAttemptAt = nil
		run.FinishedAt = &now
	}
	return run, nil
}
