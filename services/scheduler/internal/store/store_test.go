package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/schedule"
	"github.com/lush/blowball/internal/scheduledcontract"
)

func setupStore(t *testing.T) (*Store, func()) {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL-backed scheduler store test")
	}
	st, err := New(dsn)
	require.NoError(t, err)
	lockSchedulerTestSchema(t, st)
	ctx := context.Background()
	for _, tbl := range []string{"scheduled_task_runs", "scheduled_tasks"} {
		_, _ = st.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl)
	}
	execSQLFile(t, st, "../../../../migrations/026_scheduled_agent_tasks.sql")
	execSQLFile(t, st, "../../../../migrations/027_scheduled_service_fields.sql")
	cleanup := func() {
		_, _ = st.db.ExecContext(ctx, "DROP TABLE IF EXISTS scheduled_task_runs")
		_, _ = st.db.ExecContext(ctx, "DROP TABLE IF EXISTS scheduled_tasks")
		_ = st.Close()
	}
	return st, cleanup
}

const schedulerTestSchemaLock = "blowball_test_store_schema"

func lockSchedulerTestSchema(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	conn, err := st.db.Connx(ctx)
	require.NoError(t, err)
	var acquired sql.NullInt64
	require.NoError(t, conn.GetContext(ctx, &acquired, "SELECT GET_LOCK(?, 60)", schedulerTestSchemaLock))
	require.True(t, acquired.Valid && acquired.Int64 == 1, "could not acquire the MySQL test schema lock")
	t.Cleanup(func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(releaseCtx, "SELECT RELEASE_LOCK(?)", schedulerTestSchemaLock)
		_ = conn.Close()
	})
}

func execSQLFile(t *testing.T, st *Store, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var stmt strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		stmt.WriteString(line)
		stmt.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			if _, err := st.db.ExecContext(context.Background(), stmt.String()); err != nil {
				t.Fatalf("apply %s: %v\nstatement: %s", path, err, stmt.String())
			}
			stmt.Reset()
		}
	}
}

func seedTask(t *testing.T, st *Store, rule schedule.Rule, next time.Time, mutate func(*model.ScheduledTask)) *model.ScheduledTask {
	t.Helper()
	task := &model.ScheduledTask{
		TaskID: uuid.NewString(), UserID: "user-1", Name: "task", Prompt: "do it",
		SessionMode: model.ScheduledSessionNew, Enabled: true, Revision: 1,
	}
	task.SetSchedule(rule)
	task.NextRunAt = &next
	if mutate != nil {
		mutate(task)
	}
	require.NoError(t, st.CreateTask(context.Background(), task))
	return task
}

func cronRule() schedule.Rule {
	return schedule.Rule{Type: schedule.TypeCron, Expression: "* * * * *", Timezone: "UTC"}
}

func TestMaterializeCoalescesMissedCronSlots(t *testing.T) {
	st, cleanup := setupStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-10 * time.Minute)
	task := seedTask(t, st, cronRule(), old, nil)

	runs, err := st.MaterializeTasks(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1, "all missed slots coalesce to the earliest saved slot")
	assert.Equal(t, old.UTC(), runs[0].ScheduledFor.UTC())
	assert.Equal(t, model.ScheduledRunPending, runs[0].Status)
	assert.Equal(t, string(model.SessionSourceScheduled), runs[0].SessionSource)
	assert.NotEmpty(t, runs[0].SessionID)

	loaded, err := st.GetTaskInternal(ctx, task.TaskID)
	require.NoError(t, err)
	require.NotNil(t, loaded.NextRunAt)
	assert.True(t, loaded.NextRunAt.After(now), "cron advances strictly after now")

	again, err := st.MaterializeTasks(ctx, now, 10)
	require.NoError(t, err)
	assert.Empty(t, again)
}

func TestMaterializeUniqueSlotUnderConcurrency(t *testing.T) {
	st, cleanup := setupStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	seedTask(t, st, cronRule(), now.Add(-time.Minute), nil)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = st.MaterializeTasks(ctx, now, 10)
		}()
	}
	wg.Wait()

	var n int
	require.NoError(t, st.db.GetContext(ctx, &n, "SELECT COUNT(*) FROM scheduled_task_runs"))
	assert.Equal(t, 1, n, "the unique (task_id, scheduled_for) slot admits one occurrence")
}

func TestMaterializeAtClearsNextRunAndNeverReplays(t *testing.T) {
	st, cleanup := setupStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	task := seedTask(t, st, schedule.Rule{
		Type: schedule.TypeAt, RunAt: now.Add(-time.Minute).Format(time.RFC3339), Timezone: "UTC",
	}, now.Add(-time.Minute), nil)

	runs, err := st.MaterializeTasks(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	loaded, err := st.GetTaskInternal(ctx, task.TaskID)
	require.NoError(t, err)
	assert.Nil(t, loaded.NextRunAt, "a consumed at is cleared and never replayed")

	again, err := st.MaterializeTasks(ctx, now, 10)
	require.NoError(t, err)
	assert.Empty(t, again)
}

func TestMaterializeOverlapHoldAndLegacyPolicy(t *testing.T) {
	st, cleanup := setupStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	// Overlap: an active non-terminal occurrence exists.
	overlapTask := seedTask(t, st, cronRule(), now.Add(-time.Minute), nil)
	_, err := st.MaterializeTasks(ctx, now, 10)
	require.NoError(t, err)
	// Re-arm the slot and materialize again while the first is pending.
	_, err = st.db.ExecContext(ctx, "UPDATE scheduled_tasks SET next_run_at = ? WHERE task_id = ?", now.Add(-time.Second), overlapTask.TaskID)
	require.NoError(t, err)
	runs, err := st.MaterializeTasks(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, model.ScheduledRunSkipped, runs[0].Status)
	assert.Equal(t, model.ScheduledReasonTaskOverlap, runs[0].Reason)

	// Hold: a task-level hold skips every later slot.
	holdTask := seedTask(t, st, cronRule(), now.Add(-time.Minute), func(t *model.ScheduledTask) {
		t.BlockedReason = model.ScheduledReasonResultUnknown
	})
	require.NoError(t, st.SetHold(ctx, holdTask.TaskID, "held-occurrence", model.ScheduledReasonResultUnknown, now))
	runs, err = st.MaterializeTasks(ctx, now, 10)
	require.NoError(t, err)
	var held *model.ScheduledTaskRun
	for i := range runs {
		if runs[i].TaskID == holdTask.TaskID {
			held = &runs[i]
		}
	}
	require.NotNil(t, held)
	assert.Equal(t, model.ScheduledRunSkipped, held.Status)
	assert.Equal(t, model.ScheduledReasonExecutionUncertain, held.Reason)

	// Legacy existing-session policy never executes.
	legacyTask := seedTask(t, st, cronRule(), now.Add(-time.Minute), func(t *model.ScheduledTask) {
		t.SessionMode = model.ScheduledSessionExisting
		t.SessionID = strPtr("legacy-session")
	})
	runs, err = st.MaterializeTasks(ctx, now, 10)
	require.NoError(t, err)
	var legacy *model.ScheduledTaskRun
	for i := range runs {
		if runs[i].TaskID == legacyTask.TaskID {
			legacy = &runs[i]
		}
	}
	require.NotNil(t, legacy)
	assert.Equal(t, model.ScheduledRunSkipped, legacy.Status)
	assert.Equal(t, model.ScheduledSessionPolicyMigrationReason, legacy.Reason)
}

func TestClaimRunBoundedRetriesThenExhausted(t *testing.T) {
	st, cleanup := setupStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	seedTask(t, st, cronRule(), now.Add(-time.Minute), nil)
	runs, err := st.MaterializeTasks(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	occ := runs[0]

	for attempt := 1; attempt <= 3; attempt++ {
		claimed, ok, err := st.ClaimRun(ctx, occ.OccurrenceID, now, 30*time.Second, 3)
		require.NoError(t, err)
		require.True(t, ok, "attempt %d", attempt)
		assert.Equal(t, attempt, claimed.AttemptCount)
		ok, err = st.MarkLaunching(ctx, occ.OccurrenceID, claimed.ClaimToken, now, 30*time.Second)
		require.NoError(t, err)
		require.True(t, ok)
		ok, err = st.ScheduleRetry(ctx, occ.OccurrenceID, claimed.ClaimToken, model.ScheduledReasonAdmission, "boom", now)
		require.NoError(t, err)
		require.True(t, ok)
	}

	claimable, err := st.ListClaimableRuns(ctx, now, 10, 3)
	require.NoError(t, err)
	assert.Empty(t, claimable, "the persistent attempt budget is exhausted")

	exhausted, err := st.ListExhaustedRuns(ctx, 3, 10)
	require.NoError(t, err)
	require.Len(t, exhausted, 1)
	ok, err := st.FinishExhausted(ctx, exhausted[0].OccurrenceID,
		model.ScheduledRunError, model.ScheduledReasonStartRejected, "budget exhausted", now)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestAuthorizeStartGrantAndPauseRace(t *testing.T) {
	st, cleanup := setupStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	task := seedTask(t, st, cronRule(), now.Add(-time.Minute), nil)
	runs, err := st.MaterializeTasks(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	occ := runs[0]

	claimed, ok, err := st.ClaimRun(ctx, occ.OccurrenceID, now, 30*time.Second, 3)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = st.MarkLaunching(ctx, occ.OccurrenceID, claimed.ClaimToken, now, 30*time.Second)
	require.NoError(t, err)
	require.True(t, ok)

	grant, err := st.AuthorizeStart(ctx, occ.OccurrenceID, occ.RunID, occ.SessionID, claimed.AttemptCount, 3, "worker-a", now, 10*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, uint64(3), grant.Generation)
	assert.Equal(t, now.UTC(), grant.StartedAt.UTC())
	assert.Equal(t, now.Add(10*time.Minute).UTC(), grant.Deadline.UTC())

	// Idempotent retry returns the SAME grant.
	again, err := st.AuthorizeStart(ctx, occ.OccurrenceID, occ.RunID, occ.SessionID, claimed.AttemptCount, 3, "worker-a", now.Add(time.Minute), 10*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, grant.StartedAt.UTC(), again.StartedAt.UTC())

	// A different generation is fenced.
	_, err = st.AuthorizeStart(ctx, occ.OccurrenceID, occ.RunID, occ.SessionID, claimed.AttemptCount, 4, "worker-b", now, 10*time.Minute)
	assert.ErrorIs(t, err, ErrGrantFenced)

	// Pause wins a subsequent startup race.
	paused := seedTask(t, st, cronRule(), now.Add(-time.Minute), nil)
	pausedRuns, err := st.MaterializeTasks(ctx, now, 10)
	require.NoError(t, err)
	var pausedOcc *model.ScheduledTaskRun
	for i := range pausedRuns {
		if pausedRuns[i].TaskID == paused.TaskID {
			pausedOcc = &pausedRuns[i]
		}
	}
	require.NotNil(t, pausedOcc)
	claimed2, ok, err := st.ClaimRun(ctx, pausedOcc.OccurrenceID, now, 30*time.Second, 3)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = st.MarkLaunching(ctx, pausedOcc.OccurrenceID, claimed2.ClaimToken, now, 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, st.PauseTask(ctx, paused.TaskID, model.ScheduledReasonTaskPaused, now))
	_, err = st.AuthorizeStart(ctx, pausedOcc.OccurrenceID, pausedOcc.RunID, pausedOcc.SessionID, claimed2.AttemptCount, 1, "worker", now, 10*time.Minute)
	assert.ErrorIs(t, err, ErrTaskUnavailable)

	// Delete wins the same race.
	_, err = st.DeleteTask(ctx, task.UserID, task.TaskID, now)
	require.NoError(t, err)
	_, err = st.AuthorizeStart(ctx, occ.OccurrenceID, occ.RunID, occ.SessionID, claimed.AttemptCount, 3, "worker", now, 10*time.Minute)
	assert.ErrorIs(t, err, ErrTaskUnavailable)
}

func TestExecutionHoldSetAndCASClear(t *testing.T) {
	st, cleanup := setupStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	task := seedTask(t, st, cronRule(), now.Add(-time.Minute), nil)
	require.NoError(t, st.SetHold(ctx, task.TaskID, "occ-1", model.ScheduledReasonResultUnknown, now))
	loaded, err := st.GetTaskInternal(ctx, task.TaskID)
	require.NoError(t, err)
	assert.Equal(t, "occ-1", loaded.ExecutionHoldOccurrenceID)

	ok, err := st.ClearHold(ctx, task.TaskID, "occ-other")
	require.NoError(t, err)
	assert.False(t, ok, "a non-matching stop proof cannot clear the hold")

	holdTask := seedTask(t, st, cronRule(), now.Add(-time.Minute), func(t *model.ScheduledTask) {
		t.BlockedReason = model.ScheduledReasonResultUnknown
	})
	require.NoError(t, st.SetHold(ctx, holdTask.TaskID, "occ-1", model.ScheduledReasonResultUnknown, now))
	runs, err := st.MaterializeTasks(ctx, now, 10)
	require.NoError(t, err)
	var held *model.ScheduledTaskRun
	for i := range runs {
		if runs[i].TaskID == holdTask.TaskID {
			held = &runs[i]
		}
	}
	require.NotNil(t, held)
	assert.Equal(t, model.ScheduledRunSkipped, held.Status)

	ok, err = st.ClearHold(ctx, holdTask.TaskID, "occ-1")
	require.NoError(t, err)
	assert.True(t, ok)
	loaded, err = st.GetTaskInternal(ctx, holdTask.TaskID)
	require.NoError(t, err)
	assert.Empty(t, loaded.ExecutionHoldOccurrenceID)
}

func TestFinishOccurrenceIsIdempotentAndStaleSafe(t *testing.T) {
	st, cleanup := setupStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	seedTask(t, st, cronRule(), now.Add(-time.Minute), nil)
	runs, err := st.MaterializeTasks(ctx, now, 10)
	require.NoError(t, err)
	occ := runs[0]

	ok, err := st.FinishOccurrence(ctx, occ.OccurrenceID, model.ScheduledRunSucceeded, "", "", 2, now)
	require.NoError(t, err)
	require.True(t, ok)

	// A stale projection cannot overwrite the terminal fact.
	ok, err = st.FinishOccurrence(ctx, occ.OccurrenceID, model.ScheduledRunError, "late", "", 1, now)
	require.NoError(t, err)
	assert.False(t, ok)
	// Repeating the same terminal fact is idempotent.
	ok, err = st.FinishOccurrence(ctx, occ.OccurrenceID, model.ScheduledRunSucceeded, "", "", 2, now)
	require.NoError(t, err)
	assert.True(t, ok)

	loaded, err := st.GetOccurrence(ctx, occ.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledRunSucceeded, loaded.Status)
	assert.Equal(t, uint64(2), loaded.TerminalReceiptGeneration)
}

func TestUpdateTaskRevisionCASHoldPreserved(t *testing.T) {
	st, cleanup := setupStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	task := seedTask(t, st, cronRule(), now, nil)
	require.NoError(t, st.SetHold(ctx, task.TaskID, "occ-1", model.ScheduledReasonResultUnknown, now))

	task.Name = "renamed"
	next, err := st.UpdateTask(ctx, task, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(2), next)

	loaded, err := st.GetTaskInternal(ctx, task.TaskID)
	require.NoError(t, err)
	assert.Equal(t, "renamed", loaded.Name)
	assert.Equal(t, int64(2), loaded.Revision)
	assert.Equal(t, "occ-1", loaded.ExecutionHoldOccurrenceID, "server-owned hold survives config edits")

	_, err = st.UpdateTask(ctx, task, 1)
	assert.ErrorIs(t, err, scheduledcontract.ErrRevisionConflict)
}

func TestDeleteTaskCancelsUnstartedKeepsStarted(t *testing.T) {
	st, cleanup := setupStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	task := seedTask(t, st, cronRule(), now.Add(-time.Minute), nil)
	runs, err := st.MaterializeTasks(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	started := runs[0]

	// Give the occurrence a start grant (started content must survive).
	claimed, ok, err := st.ClaimRun(ctx, started.OccurrenceID, now, 30*time.Second, 3)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = st.MarkLaunching(ctx, started.OccurrenceID, claimed.ClaimToken, now, 30*time.Second)
	require.NoError(t, err)
	_, err = st.AuthorizeStart(ctx, started.OccurrenceID, started.RunID, started.SessionID, claimed.AttemptCount, 1, "worker", now, 10*time.Minute)
	require.NoError(t, err)

	// A second, unstarted occurrence exists for the same task.
	_, err = st.db.ExecContext(ctx, "UPDATE scheduled_tasks SET next_run_at = ? WHERE task_id = ?", now.Add(-time.Second), task.TaskID)
	require.NoError(t, err)
	// Bypass overlap by inserting directly.
	_, err = st.db.ExecContext(ctx, `INSERT INTO scheduled_task_runs
		(occurrence_id, task_id, user_id, revision, snapshot_json, scheduled_for, run_id, session_id, session_mode, session_source, status)
		VALUES (?, ?, ?, 1, '{}', ?, ?, ?, 'new', 'scheduled', 'pending')`,
		"pending-occ", task.TaskID, task.UserID, now, "pending-run", "pending-session")
	require.NoError(t, err)

	ok, err = st.DeleteTask(ctx, task.UserID, task.TaskID, now)
	require.NoError(t, err)
	require.True(t, ok)
	pending, err := st.GetOccurrence(ctx, "pending-occ")
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledRunCancelled, pending.Status)
	assert.Equal(t, model.ScheduledReasonTaskDeleted, pending.Reason)

	running, err := st.GetOccurrence(ctx, started.OccurrenceID)
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledRunRunning, running.Status, "an already-started execution is not cancelled by task deletion")
}

func strPtr(v string) *string { return &v }

var _ = fmt.Sprintf
