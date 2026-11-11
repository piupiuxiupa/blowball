// Package store is the Scheduler's exclusive MySQL persistence for task
// definitions, occurrences and scheduling state. It has no dependency on the
// main service's session/message stores, Agent, LLM, MCP or workspace code.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql" // register the mysql driver
	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/scheduledcontract"
)

// Store wraps the Scheduler-owned tables only.
type Store struct {
	db *sqlx.DB
}

// New opens the Scheduler's MySQL connection.
func New(dsn string) (*Store, error) {
	db, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("scheduler store: connect: %w", err)
	}
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(time.Hour)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	return &Store{db: db}, nil
}

// Close releases the pool.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the pool for health checks.
func (s *Store) DB() *sqlx.DB { return s.db }

const scheduledTaskColumns = `task_id, user_id, name, prompt, schedule_type, run_at,
	cron_expression, timezone, session_mode, session_id, model, reasoning_effort,
	enabled, next_run_at, revision, paused_reason,
	resource_revision, manifest_hash, COALESCE(execution_hold_occurrence_id, '') AS execution_hold_occurrence_id,
	blocked_reason, deleted_at, update_time, create_time`

const scheduledRunColumns = `id, occurrence_id, task_id, user_id, revision, snapshot_json,
	resource_revision, manifest_hash, scheduled_for, run_id, session_id, session_mode,
	session_source, status, reason, claim_token, lease_until, attempt_count, next_attempt_at,
	grant_generation, grant_started_at, grant_deadline, runtime_worker_generation,
	receipt_generation, terminal_receipt_generation, started_at, finished_at,
	COALESCE(last_error, '') AS last_error, update_time, create_time`

func page(limit, offset int) (int, int) {
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

func utc(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

func ptrUTC(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

func hydrateTask(task *model.ScheduledTask) {
	task.HydrateDerived()
	task.RunAt = ptrUTC(task.RunAt)
	task.NextRunAt = ptrUTC(task.NextRunAt)
	task.DeletedAt = ptrUTC(task.DeletedAt)
	task.UpdateTime = task.UpdateTime.UTC()
	task.CreateTime = task.CreateTime.UTC()
}

func hydrateRun(run *model.ScheduledTaskRun) {
	run.ScheduledFor = run.ScheduledFor.UTC()
	run.UpdateTime = run.UpdateTime.UTC()
	run.CreateTime = run.CreateTime.UTC()
	run.LeaseUntil = ptrUTC(run.LeaseUntil)
	run.NextAttemptAt = ptrUTC(run.NextAttemptAt)
	run.GrantStartedAt = ptrUTC(run.GrantStartedAt)
	run.GrantDeadline = ptrUTC(run.GrantDeadline)
	run.StartedAt = ptrUTC(run.StartedAt)
	run.FinishedAt = ptrUTC(run.FinishedAt)
}

// ErrRevisionConflict is kept as a package alias for callers that have not
// yet moved to the shared sentinel.
var ErrRevisionConflict = scheduledcontract.ErrRevisionConflict

// CreateTask inserts a task definition.
func (s *Store) CreateTask(ctx context.Context, task *model.ScheduledTask) error {
	const query = `INSERT INTO scheduled_tasks (
		task_id, user_id, name, prompt, schedule_type, run_at, cron_expression, timezone,
		session_mode, session_id, model, reasoning_effort, enabled, next_run_at, revision,
		paused_reason, resource_revision, manifest_hash
	) VALUES (
		:task_id, :user_id, :name, :prompt, :schedule_type, :run_at, :cron_expression, :timezone,
		:session_mode, :session_id, :model, :reasoning_effort, :enabled, :next_run_at, :revision,
		:paused_reason, :resource_revision, :manifest_hash
	)`
	if task.Revision <= 0 {
		task.Revision = 1
	}
	_, err := sqlx.NamedExecContext(ctx, s.db, query, task)
	return err
}

// GetTask returns one live owner task, or (nil, nil).
func (s *Store) GetTask(ctx context.Context, userID, taskID string) (*model.ScheduledTask, error) {
	query := `SELECT ` + scheduledTaskColumns + ` FROM scheduled_tasks
		WHERE user_id = ? AND task_id = ? AND deleted_at IS NULL LIMIT 1`
	var task model.ScheduledTask
	err := s.db.GetContext(ctx, &task, query, userID, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	hydrateTask(&task)
	return &task, nil
}

// GetTaskInternal reads a task including soft-deleted rows. Scheduler
// authorization/recovery use it; owner-facing reads use GetTask.
func (s *Store) GetTaskInternal(ctx context.Context, taskID string) (*model.ScheduledTask, error) {
	query := `SELECT ` + scheduledTaskColumns + ` FROM scheduled_tasks WHERE task_id = ? LIMIT 1`
	var task model.ScheduledTask
	err := s.db.GetContext(ctx, &task, query, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	hydrateTask(&task)
	return &task, nil
}

// ListTasks returns one owner page newest-first.
func (s *Store) ListTasks(ctx context.Context, userID string, limit, offset int) ([]model.ScheduledTask, error) {
	limit, offset = page(limit, offset)
	query := `SELECT ` + scheduledTaskColumns + ` FROM scheduled_tasks
		WHERE user_id = ? AND deleted_at IS NULL
		ORDER BY create_time DESC, task_id DESC LIMIT ? OFFSET ?`
	var tasks []model.ScheduledTask
	if err := s.db.SelectContext(ctx, &tasks, query, userID, limit, offset); err != nil {
		return nil, err
	}
	for i := range tasks {
		hydrateTask(&tasks[i])
	}
	return tasks, nil
}

// UpdateTask applies a configuration update with an optional revision CAS.
// The server-owned execution hold columns are never touched here.
func (s *Store) UpdateTask(ctx context.Context, task *model.ScheduledTask, expectedRevision int64) (int64, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	const update = `UPDATE scheduled_tasks SET
		name = ?, prompt = ?, schedule_type = ?, run_at = ?, cron_expression = ?, timezone = ?,
		session_mode = ?, session_id = ?, model = ?, reasoning_effort = ?, enabled = ?,
		next_run_at = ?, paused_reason = ?, resource_revision = ?, manifest_hash = ?,
		revision = revision + 1, update_time = NOW()
		WHERE user_id = ? AND task_id = ? AND deleted_at IS NULL`
	query := update
	args := []any{
		task.Name, task.Prompt, task.ScheduleType, task.RunAt, task.CronExpression, task.Timezone,
		task.SessionMode, task.SessionID, task.Model, task.ReasoningEffort, task.Enabled,
		task.NextRunAt, task.PausedReason, task.ResourceRevision, task.ManifestHash,
		task.UserID, task.TaskID,
	}
	if expectedRevision > 0 {
		query += ` AND revision = ?`
		args = append(args, expectedRevision)
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if affected == 0 {
		var current int64
		if err := tx.GetContext(ctx, &current, `SELECT revision FROM scheduled_tasks
			WHERE user_id = ? AND task_id = ? AND deleted_at IS NULL`, task.UserID, task.TaskID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, nil
			}
			return 0, err
		}
		return 0, ErrRevisionConflict
	}
	if !task.Enabled {
		reason := task.PausedReason
		if reason == "" {
			reason = model.ScheduledReasonTaskPaused
		}
		if err := cancelUnstarted(ctx, tx, task.TaskID, reason, time.Now().UTC()); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return task.Revision + 1, nil
}

// DeleteTask soft-deletes a task and cancels every occurrence that has not
// crossed the start fence. Already running content/resources stay untouched.
func (s *Store) DeleteTask(ctx context.Context, userID, taskID string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	const update = `UPDATE scheduled_tasks SET deleted_at = ?, enabled = 0, next_run_at = NULL,
		revision = revision + 1, update_time = NOW()
		WHERE user_id = ? AND task_id = ? AND deleted_at IS NULL`
	res, err := tx.ExecContext(ctx, update, utc(now), userID, taskID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	if err := cancelUnstarted(ctx, tx, taskID, model.ScheduledReasonTaskDeleted, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func cancelUnstarted(ctx context.Context, tx *sqlx.Tx, taskID, reason string, now time.Time) error {
	const query = `UPDATE scheduled_task_runs SET status = 'cancelled', reason = ?,
		finished_at = ?, claim_token = '', lease_until = NULL, next_attempt_at = NULL, update_time = NOW()
		WHERE task_id = ? AND started_at IS NULL AND grant_generation = 0
		  AND status IN ('pending','preparing','launching')`
	_, err := tx.ExecContext(ctx, query, reason, utc(now), taskID)
	return err
}

// PauseTask pauses a task after an authorization failure (owner unavailable).
func (s *Store) PauseTask(ctx context.Context, taskID, reason string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scheduled_tasks SET enabled = 0, paused_reason = ?,
		next_run_at = NULL, revision = revision + 1, update_time = NOW()
		WHERE task_id = ? AND deleted_at IS NULL`, reason, taskID)
	return err
}

// ListOccurrences returns a task's history regardless of the task's live
// status: a deleted task keeps its recorded execution facts readable. Rows are
// owner-filtered by the denormalized user_id.
func (s *Store) ListOccurrences(ctx context.Context, userID, taskID string, limit, offset int) ([]model.ScheduledTaskRun, error) {
	limit, offset = page(limit, offset)
	query := `SELECT ` + scheduledRunColumns + ` FROM scheduled_task_runs
		WHERE user_id = ? AND task_id = ?
		ORDER BY scheduled_for DESC, id DESC LIMIT ? OFFSET ?`
	var runs []model.ScheduledTaskRun
	if err := s.db.SelectContext(ctx, &runs, query, userID, taskID, limit, offset); err != nil {
		return nil, err
	}
	for i := range runs {
		hydrateRun(&runs[i])
	}
	return runs, nil
}

// GetOccurrence returns one authoritative occurrence.
func (s *Store) GetOccurrence(ctx context.Context, occurrenceID string) (*model.ScheduledTaskRun, error) {
	query := `SELECT ` + scheduledRunColumns + ` FROM scheduled_task_runs WHERE occurrence_id = ? LIMIT 1`
	var run model.ScheduledTaskRun
	err := s.db.GetContext(ctx, &run, query, occurrenceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	hydrateRun(&run)
	return &run, nil
}

// ResourceReferences reports whether a published resource revision is still
// pinned by a live task or by any retained occurrence. The Scheduler owns both
// tables, so this is the authoritative reference check for main-service
// maintenance.
func (s *Store) ResourceReferences(ctx context.Context, taskID string, revision uint64) (bool, error) {
	var n int
	if err := s.db.GetContext(ctx, &n, `SELECT COUNT(*) FROM scheduled_tasks
		WHERE task_id = ? AND resource_revision = ?`, taskID, revision); err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	if err := s.db.GetContext(ctx, &n, `SELECT COUNT(*) FROM scheduled_task_runs
		WHERE task_id = ? AND resource_revision = ?`, taskID, revision); err != nil {
		return false, err
	}
	return n > 0, nil
}
