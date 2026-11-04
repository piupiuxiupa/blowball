package mysql

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/schedule"
)

func setupScheduledTaskStore(t *testing.T) *Store {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL-backed scheduled task test")
	}
	cfg, err := drivermysql.ParseDSN(dsn)
	require.NoError(t, err)
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	store, err := New(cfg.FormatDSN())
	require.NoError(t, err)
	ctx := context.Background()
	cleanup := func() {
		for _, table := range []string{"scheduled_task_runs", "scheduled_tasks", "sessions", "users"} {
			_, _ = store.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+table)
		}
	}
	cleanup()
	t.Cleanup(func() { cleanup(); _ = store.Close() })
	for _, path := range []string{"../../../migrations/001_users.sql", "../../../migrations/002_sessions.sql", "../../../migrations/026_scheduled_agent_tasks.sql"} {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		var sqlText strings.Builder
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				sqlText.WriteString(line)
				sqlText.WriteByte('\n')
			}
		}
		for _, statement := range strings.Split(sqlText.String(), ";\n") {
			if strings.TrimSpace(statement) != "" {
				_, err = store.db.ExecContext(ctx, statement)
				require.NoError(t, err, path)
			}
		}
	}
	for _, userID := range []string{"user-1", "user-2"} {
		_, err = store.db.ExecContext(ctx, `INSERT INTO users (user_id, username, password, status) VALUES (?, ?, 'hash', 'active')`, userID, userID)
		require.NoError(t, err)
	}
	return store
}

func scheduledTaskFixture(taskID, userID string) *model.ScheduledTask {
	task := &model.ScheduledTask{
		TaskID: taskID, UserID: userID, Name: "report", Prompt: "daily report",
		SessionMode: model.ScheduledSessionNew, Enabled: true, Revision: 1,
	}
	task.SetSchedule(schedule.Rule{Type: schedule.TypeCron, Expression: "30 9 * * *", Timezone: "UTC"})
	next := time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)
	task.NextRunAt = &next
	task.ApplyDefaults()
	return task
}

func insertScheduledRunFixture(t *testing.T, store *Store, run *model.ScheduledTaskRun) {
	t.Helper()
	_, err := store.db.ExecContext(context.Background(), `INSERT INTO scheduled_task_runs (
		occurrence_id, task_id, user_id, revision, snapshot_json, scheduled_for, run_id, session_id,
		session_mode, status, reason, next_attempt_at, started_at, finished_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.OccurrenceID, run.TaskID, run.UserID, run.Revision, run.SnapshotJSON, run.ScheduledFor,
		run.RunID, run.SessionID, run.SessionMode, run.Status, run.Reason, run.NextAttemptAt, run.StartedAt, run.FinishedAt)
	require.NoError(t, err)
}

func TestScheduledTaskStoreCRUDAndPagedHistory(t *testing.T) {
	store := setupScheduledTaskStore(t)
	ctx := context.Background()
	first := scheduledTaskFixture("task-1", "user-1")
	second := scheduledTaskFixture("task-2", "user-2")
	require.NoError(t, store.CreateScheduledTask(ctx, first))
	require.NoError(t, store.CreateScheduledTask(ctx, second))

	got, err := store.GetScheduledTask(ctx, "user-2", first.TaskID)
	require.NoError(t, err)
	require.Nil(t, got, "cross-user reads must not disclose the task")
	got, err = store.GetScheduledTask(ctx, "user-1", first.TaskID)
	require.NoError(t, err)
	require.Equal(t, first.TaskID, got.TaskID)
	require.Equal(t, model.ScheduledSessionNew, got.Session.Mode)

	listed, err := store.ListScheduledTasks(ctx, "user-1", 20, 0)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	first.Name = "updated"
	first.Prompt = "new prompt"
	updated, err := store.UpdateScheduledTask(ctx, first)
	require.NoError(t, err)
	require.True(t, updated)
	require.Equal(t, int64(2), first.Revision)

	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		run, err := newScheduledTaskRun(first, now.Add(time.Duration(i)*time.Minute), now, false)
		require.NoError(t, err)
		run.OccurrenceID = fmt.Sprintf("occurrence-%d", i)
		run.RunID = fmt.Sprintf("run-%d", i)
		run.SessionID = fmt.Sprintf("session-%d", i)
		run.Status = model.ScheduledRunSucceeded
		insertScheduledRunFixture(t, store, run)
	}
	page, err := store.ListScheduledTaskRuns(ctx, "user-1", first.TaskID, 2, 0)
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, "occurrence-3", page[0].OccurrenceID)
	require.Equal(t, "occurrence-2", page[1].OccurrenceID)
	page, err = store.ListScheduledTaskRuns(ctx, "user-1", first.TaskID, 2, 2)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, "occurrence-1", page[0].OccurrenceID)

	deleted, err := store.DeleteScheduledTask(ctx, "user-2", first.TaskID, now)
	require.NoError(t, err)
	require.False(t, deleted)
	deleted, err = store.DeleteScheduledTask(ctx, "user-1", first.TaskID, now)
	require.NoError(t, err)
	require.True(t, deleted)
	got, err = store.GetScheduledTask(ctx, "user-1", first.TaskID)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestScheduledTaskMaterializationCoalescesAndRollsBack(t *testing.T) {
	store := setupScheduledTaskStore(t)
	ctx := context.Background()
	task := scheduledTaskFixture("task-cron", "user-1")
	require.NoError(t, store.CreateScheduledTask(ctx, task))
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	runs, err := store.MaterializeScheduledTasks(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1, "several missed slots coalesce to one occurrence")
	require.Equal(t, *task.NextRunAt, runs[0].ScheduledFor)
	require.Equal(t, model.ScheduledRunPending, runs[0].Status)
	require.NotEmpty(t, runs[0].RunID)
	require.NotEmpty(t, runs[0].SessionID)
	updated, err := store.GetScheduledTask(ctx, "user-1", task.TaskID)
	require.NoError(t, err)
	require.NotNil(t, updated.NextRunAt)
	require.True(t, updated.NextRunAt.After(now))
	require.Equal(t, time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC), *updated.NextRunAt)
	runs, err = store.MaterializeScheduledTasks(ctx, now, 10)
	require.NoError(t, err)
	require.Empty(t, runs)

	oneShot := scheduledTaskFixture("task-at", "user-1")
	runAt := now.Add(-time.Hour)
	oneShot.SetSchedule(schedule.Rule{Type: schedule.TypeAt, RunAt: runAt.Format(time.RFC3339), Timezone: "UTC"})
	oneShot.NextRunAt = &runAt
	require.NoError(t, store.CreateScheduledTask(ctx, oneShot))
	runs, err = store.MaterializeScheduledTasks(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, runAt, runs[0].ScheduledFor)
	oneShot, err = store.GetScheduledTask(ctx, "user-1", oneShot.TaskID)
	require.NoError(t, err)
	require.Nil(t, oneShot.NextRunAt)
	runs, err = store.MaterializeScheduledTasks(ctx, now, 10)
	require.NoError(t, err)
	require.Empty(t, runs, "a consumed one-shot is never replayed")

	broken := scheduledTaskFixture("task-broken", "user-1")
	broken.CronExpression = "not a cron"
	require.NoError(t, store.CreateScheduledTask(ctx, broken))
	_, err = store.MaterializeScheduledTasks(ctx, now, 10)
	require.Error(t, err)
	var count int
	require.NoError(t, store.db.GetContext(ctx, &count, `SELECT COUNT(*) FROM scheduled_task_runs WHERE task_id = ?`, broken.TaskID))
	require.Zero(t, count, "the occurrence insert must roll back with advancement")
	brokenAfter, err := store.GetScheduledTask(ctx, "user-1", broken.TaskID)
	require.NoError(t, err)
	require.Equal(t, *broken.NextRunAt, *brokenAfter.NextRunAt)
}

func TestScheduledTaskMaterializationConcurrentSingleSlot(t *testing.T) {
	store := setupScheduledTaskStore(t)
	ctx := context.Background()
	task := scheduledTaskFixture("task-race", "user-1")
	require.NoError(t, store.CreateScheduledTask(ctx, task))
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

	const callers = 6
	var wg sync.WaitGroup
	results := make(chan []model.ScheduledTaskRun, callers)
	errs := make(chan error, callers)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			runs, err := store.MaterializeScheduledTasks(ctx, now, 10)
			results <- runs
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	total := 0
	for runs := range results {
		total += len(runs)
	}
	require.Equal(t, 1, total, "one task slot materializes once across callers")
	var count int
	require.NoError(t, store.db.GetContext(ctx, &count, `SELECT COUNT(*) FROM scheduled_task_runs WHERE task_id = ?`, task.TaskID))
	require.Equal(t, 1, count)
}

func TestScheduledTaskMaterializationOverlap(t *testing.T) {
	for _, activeStatus := range []string{model.ScheduledRunPending, model.ScheduledRunRunning} {
		t.Run(activeStatus, func(t *testing.T) {
			store := setupScheduledTaskStore(t)
			ctx := context.Background()
			task := scheduledTaskFixture("task-overlap-"+activeStatus, "user-1")
			require.NoError(t, store.CreateScheduledTask(ctx, task))
			now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
			active, err := newScheduledTaskRun(task, now.Add(-time.Hour), now, false)
			require.NoError(t, err)
			active.Status = activeStatus
			insertScheduledRunFixture(t, store, active)

			runs, err := store.MaterializeScheduledTasks(ctx, now, 10)
			require.NoError(t, err)
			require.Len(t, runs, 1)
			require.Equal(t, model.ScheduledRunSkipped, runs[0].Status)
			require.Equal(t, model.ScheduledReasonTaskOverlap, runs[0].Reason)
			previous, err := store.GetScheduledTaskRun(ctx, active.OccurrenceID)
			require.NoError(t, err)
			require.Equal(t, activeStatus, previous.Status)
		})
	}
}

func TestScheduledRunLeaseFenceAndPersistentRetries(t *testing.T) {
	store := setupScheduledTaskStore(t)
	ctx := context.Background()
	task := scheduledTaskFixture("task-fence", "user-1")
	require.NoError(t, store.CreateScheduledTask(ctx, task))
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	runs, err := store.MaterializeScheduledTasks(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	occurrenceID := runs[0].OccurrenceID

	first, claimed, err := store.ClaimScheduledRun(ctx, occurrenceID, now, time.Minute, 3)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, 1, first.AttemptCount)
	_, claimed, err = store.ClaimScheduledRun(ctx, occurrenceID, now.Add(time.Second), time.Minute, 3)
	require.NoError(t, err)
	require.False(t, claimed)

	second, claimed, err := store.ClaimScheduledRun(ctx, occurrenceID, now.Add(2*time.Minute), time.Minute, 3)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, 2, second.AttemptCount)
	require.NotEqual(t, first.ClaimToken, second.ClaimToken)
	launched, err := store.MarkScheduledRunLaunching(ctx, occurrenceID, first.ClaimToken, now.Add(2*time.Minute))
	require.NoError(t, err)
	require.False(t, launched, "an expired claimant cannot enter the execution window")
	launched, err = store.MarkScheduledRunLaunching(ctx, occurrenceID, second.ClaimToken, now.Add(2*time.Minute))
	require.NoError(t, err)
	require.True(t, launched)
	crossed, err := store.CrossScheduledStartFence(ctx, occurrenceID, second.ClaimToken, now.Add(2*time.Minute))
	require.NoError(t, err)
	require.True(t, crossed)
	current, err := store.GetScheduledTaskRun(ctx, occurrenceID)
	require.NoError(t, err)
	require.Equal(t, model.ScheduledRunRunning, current.Status)
	require.NotNil(t, current.StartedAt)

	// A separate occurrence exercises durable retry count and next-attempt time.
	other := scheduledTaskFixture("task-retry", "user-1")
	require.NoError(t, store.CreateScheduledTask(ctx, other))
	runs, err = store.MaterializeScheduledTasks(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	occurrenceID = runs[0].OccurrenceID
	claim, claimed, err := store.ClaimScheduledRun(ctx, occurrenceID, now, time.Minute, 2)
	require.NoError(t, err)
	require.True(t, claimed)
	retryAt := now.Add(5 * time.Minute)
	scheduled, err := store.ScheduleScheduledRunRetry(ctx, occurrenceID, claim.ClaimToken, retryAt, "redis down")
	require.NoError(t, err)
	require.True(t, scheduled)
	_, claimed, err = store.ClaimScheduledRun(ctx, occurrenceID, now.Add(time.Minute), time.Minute, 2)
	require.NoError(t, err)
	require.False(t, claimed, "backoff is durable and not reset by another claim loop")
	claim, claimed, err = store.ClaimScheduledRun(ctx, occurrenceID, retryAt, time.Minute, 2)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, 2, claim.AttemptCount)
	_, claimed, err = store.ClaimScheduledRun(ctx, occurrenceID, retryAt.Add(2*time.Minute), time.Minute, 2)
	require.NoError(t, err)
	require.False(t, claimed, "the persisted attempt cap survives reclaim/restart")
}

func TestScheduledTaskUpdateLeavesMaterializedSnapshotUnchanged(t *testing.T) {
	store := setupScheduledTaskStore(t)
	ctx := context.Background()
	task := scheduledTaskFixture("task-snapshot", "user-1")
	require.NoError(t, store.CreateScheduledTask(ctx, task))
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	runs, err := store.MaterializeScheduledTasks(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	before := runs[0].SnapshotJSON

	task.Prompt = "changed later"
	task.Model = "different-model"
	updated, err := store.UpdateScheduledTask(ctx, task)
	require.NoError(t, err)
	require.True(t, updated)
	current, err := store.GetScheduledTaskRun(ctx, runs[0].OccurrenceID)
	require.NoError(t, err)
	require.Equal(t, before, current.SnapshotJSON)
	snapshot, err := current.Snapshot()
	require.NoError(t, err)
	require.Equal(t, "daily report", snapshot.Prompt)
	require.NotContains(t, current.SnapshotJSON, "sk-user-secret")
}

func TestScheduledTaskPauseCoordinatesWithStartFence(t *testing.T) {
	store := setupScheduledTaskStore(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

	for _, crossFirst := range []bool{false, true} {
		task := scheduledTaskFixture(fmt.Sprintf("task-pause-race-%v", crossFirst), "user-1")
		require.NoError(t, store.CreateScheduledTask(ctx, task))
		runs, err := store.MaterializeScheduledTasks(ctx, now, 10)
		require.NoError(t, err)
		require.Len(t, runs, 1)
		claim, claimed, err := store.ClaimScheduledRun(ctx, runs[0].OccurrenceID, now, time.Minute, 3)
		require.NoError(t, err)
		require.True(t, claimed)
		launched, err := store.MarkScheduledRunLaunching(ctx, runs[0].OccurrenceID, claim.ClaimToken, now)
		require.NoError(t, err)
		require.True(t, launched)
		if crossFirst {
			crossed, err := store.CrossScheduledStartFence(ctx, runs[0].OccurrenceID, claim.ClaimToken, now)
			require.NoError(t, err)
			require.True(t, crossed)
		}

		task.Enabled = false
		task.PausedReason = model.ScheduledReasonTaskPaused
		updated, err := store.UpdateScheduledTask(ctx, task)
		require.NoError(t, err)
		require.True(t, updated)
		crossed, err := store.CrossScheduledStartFence(ctx, runs[0].OccurrenceID, claim.ClaimToken, now)
		require.NoError(t, err)
		require.False(t, crossed, "a paused task can never cross a later fence")

		current, err := store.GetScheduledTaskRun(ctx, runs[0].OccurrenceID)
		require.NoError(t, err)
		if crossFirst {
			require.Equal(t, model.ScheduledRunRunning, current.Status, "pause must not cancel an already-fenced run")
			require.NotNil(t, current.StartedAt)
		} else {
			require.Equal(t, model.ScheduledRunCancelled, current.Status)
			require.Equal(t, model.ScheduledReasonTaskPaused, current.Reason)
			require.Nil(t, current.StartedAt)
		}
	}
}

func TestScheduledRunMissingUserAuthorization(t *testing.T) {
	store := setupScheduledTaskStore(t)
	ctx := context.Background()
	task := scheduledTaskFixture("task-user-gone", "user-1")
	require.NoError(t, store.CreateScheduledTask(ctx, task))
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	runs, err := store.MaterializeScheduledTasks(ctx, now, 10)
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `DELETE FROM users WHERE user_id = ?`, task.UserID)
	require.NoError(t, err)
	reason, err := store.ScheduledRunAuthorization(ctx, runs[0].OccurrenceID)
	require.NoError(t, err)
	require.Equal(t, model.ScheduledReasonUserUnavailable, reason)
	crossed, err := store.CrossScheduledStartFence(ctx, runs[0].OccurrenceID, "any", now)
	require.NoError(t, err)
	require.False(t, crossed)
}
