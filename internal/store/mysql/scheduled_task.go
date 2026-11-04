package mysql

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
)

const scheduledTaskColumns = `task_id, user_id, name, prompt, schedule_type, run_at,
	cron_expression, timezone, session_mode, session_id, model, reasoning_effort,
	enabled, next_run_at, revision, paused_reason, deleted_at, update_time, create_time`

const scheduledRunColumns = `id, occurrence_id, task_id, user_id, revision, snapshot_json,
	scheduled_for, run_id, session_id, session_mode, status, reason, claim_token, lease_until,
	attempt_count, next_attempt_at, started_at, finished_at, COALESCE(last_error, '') AS last_error, update_time, create_time`

func scheduledPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func hydrateScheduledTask(task *model.ScheduledTask) {
	task.HydrateDerived()
	if task.RunAt != nil {
		v := task.RunAt.UTC()
		task.RunAt = &v
	}
	if task.NextRunAt != nil {
		v := task.NextRunAt.UTC()
		task.NextRunAt = &v
	}
	if task.DeletedAt != nil {
		v := task.DeletedAt.UTC()
		task.DeletedAt = &v
	}
	task.UpdateTime = task.UpdateTime.UTC()
	task.CreateTime = task.CreateTime.UTC()
}

func hydrateScheduledRun(run *model.ScheduledTaskRun) {
	run.ScheduledFor = run.ScheduledFor.UTC()
	run.UpdateTime = run.UpdateTime.UTC()
	run.CreateTime = run.CreateTime.UTC()
	for _, value := range []**time.Time{&run.LeaseUntil, &run.NextAttemptAt, &run.StartedAt, &run.FinishedAt} {
		if *value != nil {
			v := (*value).UTC()
			*value = &v
		}
	}
}

func (s *Store) CreateScheduledTask(ctx context.Context, task *model.ScheduledTask) error {
	const query = `INSERT INTO scheduled_tasks (
		task_id, user_id, name, prompt, schedule_type, run_at, cron_expression, timezone,
		session_mode, session_id, model, reasoning_effort, enabled, next_run_at, revision, paused_reason
	) VALUES (
		:task_id, :user_id, :name, :prompt, :schedule_type, :run_at, :cron_expression, :timezone,
		:session_mode, :session_id, :model, :reasoning_effort, :enabled, :next_run_at, :revision, :paused_reason
	)`
	if task.Revision <= 0 {
		task.Revision = 1
	}
	logQuery(ctx, "scheduled_task.create", query, task.TaskID)
	_, err := sqlx.NamedExecContext(ctx, s.db, query, task)
	return err
}

func (s *Store) GetScheduledTask(ctx context.Context, userID, taskID string) (*model.ScheduledTask, error) {
	query := `SELECT ` + scheduledTaskColumns + ` FROM scheduled_tasks
		WHERE user_id = ? AND task_id = ? AND deleted_at IS NULL LIMIT 1`
	logQuery(ctx, "scheduled_task.get", query, userID, taskID)
	var task model.ScheduledTask
	err := s.db.GetContext(ctx, &task, query, userID, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	hydrateScheduledTask(&task)
	return &task, nil
}

// getScheduledTaskInternal reads a task including soft-deleted rows for
// scheduler authorization/recovery. HTTP services use GetScheduledTask.
func (s *Store) getScheduledTaskInternal(ctx context.Context, taskID string) (*model.ScheduledTask, error) {
	query := `SELECT ` + scheduledTaskColumns + ` FROM scheduled_tasks WHERE task_id = ? LIMIT 1`
	logQuery(ctx, "scheduled_task.get_internal", query, taskID)
	var task model.ScheduledTask
	err := s.db.GetContext(ctx, &task, query, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	hydrateScheduledTask(&task)
	return &task, nil
}

func (s *Store) ListScheduledTasks(ctx context.Context, userID string, limit, offset int) ([]model.ScheduledTask, error) {
	limit, offset = scheduledPage(limit, offset)
	query := `SELECT ` + scheduledTaskColumns + ` FROM scheduled_tasks
		WHERE user_id = ? AND deleted_at IS NULL
		ORDER BY create_time DESC, task_id DESC LIMIT ? OFFSET ?`
	logQuery(ctx, "scheduled_task.list", query, userID, limit, offset)
	var tasks []model.ScheduledTask
	if err := s.db.SelectContext(ctx, &tasks, query, userID, limit, offset); err != nil {
		return nil, err
	}
	for i := range tasks {
		hydrateScheduledTask(&tasks[i])
	}
	return tasks, nil
}

// UpdateScheduledTask applies a new task definition. Disabling also cancels
// every occurrence that has not crossed the startup fence; running occurrences
// and all immutable snapshots remain untouched.
func (s *Store) UpdateScheduledTask(ctx context.Context, task *model.ScheduledTask) (bool, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	const update = `UPDATE scheduled_tasks SET
		name = ?, prompt = ?, schedule_type = ?, run_at = ?, cron_expression = ?, timezone = ?,
		session_mode = ?, session_id = ?, model = ?, reasoning_effort = ?, enabled = ?,
		next_run_at = ?, paused_reason = ?, revision = revision + 1, update_time = NOW()
		WHERE user_id = ? AND task_id = ? AND deleted_at IS NULL`
	logQuery(ctx, "scheduled_task.update", update, task.UserID, task.TaskID)
	res, err := tx.ExecContext(ctx, update,
		task.Name, task.Prompt, task.ScheduleType, task.RunAt, task.CronExpression, task.Timezone,
		task.SessionMode, task.SessionID, task.Model, task.ReasoningEffort, task.Enabled,
		task.NextRunAt, task.PausedReason, task.UserID, task.TaskID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	if !task.Enabled {
		reason := task.PausedReason
		if reason == "" {
			reason = model.ScheduledReasonTaskPaused
		}
		if err := cancelUnstartedScheduledRuns(ctx, tx, task.TaskID, reason, time.Now().UTC()); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	task.Revision++
	return true, nil
}

func cancelUnstartedScheduledRuns(ctx context.Context, tx *sqlx.Tx, taskID, reason string, now time.Time) error {
	const query = `UPDATE scheduled_task_runs SET status = 'cancelled', reason = ?,
		finished_at = ?, claim_token = '', lease_until = NULL, next_attempt_at = NULL, update_time = NOW()
		WHERE task_id = ? AND started_at IS NULL AND status IN ('pending', 'preparing', 'launching')`
	_, err := tx.ExecContext(ctx, query, reason, scheduledUTC(now), taskID)
	return err
}

func (s *Store) DeleteScheduledTask(ctx context.Context, userID, taskID string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	const update = `UPDATE scheduled_tasks SET deleted_at = ?, enabled = 0, next_run_at = NULL,
		revision = revision + 1, update_time = NOW()
		WHERE user_id = ? AND task_id = ? AND deleted_at IS NULL`
	logQuery(ctx, "scheduled_task.delete", update, userID, taskID)
	res, err := tx.ExecContext(ctx, update, scheduledUTC(now), userID, taskID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	if err := cancelUnstartedScheduledRuns(ctx, tx, taskID, model.ScheduledReasonTaskDeleted, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ListScheduledTaskRuns(ctx context.Context, userID, taskID string, limit, offset int) ([]model.ScheduledTaskRun, error) {
	limit, offset = scheduledPage(limit, offset)
	query := `SELECT ` + scheduledRunColumns + ` FROM scheduled_task_runs
		WHERE user_id = ? AND task_id = ? ORDER BY scheduled_for DESC, id DESC LIMIT ? OFFSET ?`
	logQuery(ctx, "scheduled_task.list_runs", query, userID, taskID, limit, offset)
	var runs []model.ScheduledTaskRun
	if err := s.db.SelectContext(ctx, &runs, query, userID, taskID, limit, offset); err != nil {
		return nil, err
	}
	for i := range runs {
		hydrateScheduledRun(&runs[i])
	}
	return runs, nil
}

func (s *Store) GetScheduledTaskRun(ctx context.Context, occurrenceID string) (*model.ScheduledTaskRun, error) {
	query := `SELECT ` + scheduledRunColumns + ` FROM scheduled_task_runs WHERE occurrence_id = ? LIMIT 1`
	logQuery(ctx, "scheduled_task.get_run", query, occurrenceID)
	var run model.ScheduledTaskRun
	err := s.db.GetContext(ctx, &run, query, occurrenceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	hydrateScheduledRun(&run)
	return &run, nil
}

func (s *Store) GetScheduledTaskRunByRunID(ctx context.Context, runID string) (*model.ScheduledTaskRun, error) {
	query := `SELECT ` + scheduledRunColumns + ` FROM scheduled_task_runs WHERE run_id = ? LIMIT 1`
	logQuery(ctx, "scheduled_task.get_run_by_run_id", query, runID)
	var run model.ScheduledTaskRun
	err := s.db.GetContext(ctx, &run, query, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	hydrateScheduledRun(&run)
	return &run, nil
}

func scheduledUTC(value time.Time) time.Time {
	return value.UTC().Truncate(time.Millisecond)
}
