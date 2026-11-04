package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/schedule"
	"github.com/lush/blowball/internal/turn"
)

func runForTask(t *testing.T, store *fakeStore, taskID string) *model.ScheduledTaskRun {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, run := range store.runs {
		if run.TaskID == taskID {
			return cloneRun(run)
		}
	}
	t.Fatalf("no run for task %s", taskID)
	return nil
}

func TestSchedulerRunOnceCompletesDueSchedules(t *testing.T) {
	s, store, turns, _, _, clock, _ := schedulerFixture(t)
	clock.Add(10 * time.Hour)
	store.addTask(cronTask("cron"))
	at := cronTask("at")
	at.SetSchedule(schedule.Rule{Type: schedule.TypeAt, RunAt: "2026-11-04T09:00:00Z", Timezone: "UTC"})
	past := clock.Now().Add(-time.Hour)
	at.NextRunAt = &past
	store.addTask(at)
	future := cronTask("future")
	next := clock.Now().Add(time.Hour)
	future.NextRunAt = &next
	store.addTask(future)

	require.NoError(t, s.RunOnce(context.Background()))
	waitSchedulerIdle(t, s)
	require.Equal(t, model.ScheduledRunSucceeded, runForTask(t, store, "cron").Status)
	require.Equal(t, model.ScheduledRunSucceeded, runForTask(t, store, "at").Status)
	require.Nil(t, store.tasks["at"].NextRunAt)
	require.Equal(t, time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC), *store.tasks["cron"].NextRunAt)
	require.Len(t, store.runs, 2, "future slots are not materialized early")
	require.Len(t, turns.requests, 2)
	require.Equal(t, "user-1", turns.userIDs[0])
}

func TestSchedulerAdmissionRetriesPersistAcrossRestart(t *testing.T) {
	s, store, turns, sessions, tokens, clock, manager := schedulerFixture(t)
	clock.Add(10 * time.Hour)
	store.addTask(cronTask("retry"))
	turns.outcome = turn.SubmitAdmissionFailed
	turns.err = errors.New("redis down")

	require.NoError(t, s.RunOnce(context.Background()))
	waitSchedulerIdle(t, s)
	first := runForTask(t, store, "retry")
	require.Equal(t, model.ScheduledRunPending, first.Status)
	require.Equal(t, 1, first.AttemptCount)
	require.Equal(t, clock.Now().Add(time.Second), *first.NextAttemptAt)

	clock.Add(time.Second)
	restarted := New(Dependencies{
		Store: store, Turns: turns, Sessions: sessions, Tokens: tokens, Runs: manager,
		ModelSelection: s.selection, Now: clock.Now, Config: s.cfg,
	})
	require.NoError(t, restarted.RunOnce(context.Background()))
	waitSchedulerIdle(t, restarted)
	second := runForTask(t, store, "retry")
	require.Equal(t, 2, second.AttemptCount, "restart does not reset the persisted attempt count")

	clock.Add(2 * time.Second)
	require.NoError(t, restarted.RunOnce(context.Background()))
	waitSchedulerIdle(t, restarted)
	third := runForTask(t, store, "retry")
	require.Equal(t, model.ScheduledRunError, third.Status)
	require.Equal(t, 3, third.AttemptCount)
	require.Equal(t, model.ScheduledReasonAdmission, third.Reason)
	require.Len(t, turns.requests, 3, "no fourth submission occurs after the attempt cap")
}

func TestSchedulerAuthorizationFailurePausesBeforeModel(t *testing.T) {
	for _, reason := range []string{model.ScheduledReasonUserUnavailable, model.ScheduledReasonSessionUnavailable} {
		t.Run(reason, func(t *testing.T) {
			s, store, turns, sessions, _, clock, _ := schedulerFixture(t)
			clock.Add(10 * time.Hour)
			store.addTask(cronTask("unavailable"))
			store.authReason["unavailable"] = reason
			require.NoError(t, s.RunOnce(context.Background()))
			waitSchedulerIdle(t, s)
			run := runForTask(t, store, "unavailable")
			require.Equal(t, model.ScheduledRunSkipped, run.Status)
			require.Equal(t, reason, run.Reason)
			require.False(t, store.tasks["unavailable"].Enabled)
			require.Empty(t, turns.requests)
			require.Empty(t, sessions.calls)
		})
	}
}

func TestSchedulerPreparesReservedSessionIdempotently(t *testing.T) {
	s, store, turns, sessions, _, clock, _ := schedulerFixture(t)
	clock.Add(10 * time.Hour)
	store.addTask(cronTask("prepare"))
	turns.outcome = turn.SubmitAdmissionFailed
	turns.err = errors.New("temporary")
	require.NoError(t, s.RunOnce(context.Background()))
	waitSchedulerIdle(t, s)
	reserved := runForTask(t, store, "prepare").SessionID

	turns.outcome = turn.SubmitStarted
	clock.Add(time.Second)
	require.NoError(t, s.RunOnce(context.Background()))
	waitSchedulerIdle(t, s)
	require.Equal(t, model.ScheduledRunSucceeded, runForTask(t, store, "prepare").Status)
	require.Equal(t, []string{reserved, reserved}, sessions.calls)
	require.Len(t, sessions.byID, 1, "retrying preparation never creates a second session")
}

func TestSchedulerBusyAndUncertainFenceOutcomes(t *testing.T) {
	t.Run("busy", func(t *testing.T) {
		s, store, turns, _, _, clock, _ := schedulerFixture(t)
		clock.Add(10 * time.Hour)
		store.addTask(cronTask("busy"))
		turns.outcome = turn.SubmitSessionBusy
		turns.err = turn.ErrSessionBusy
		require.NoError(t, s.RunOnce(context.Background()))
		waitSchedulerIdle(t, s)
		require.Equal(t, model.ScheduledRunSkipped, runForTask(t, store, "busy").Status)
		require.Equal(t, model.ScheduledReasonSessionBusy, runForTask(t, store, "busy").Reason)
	})

	t.Run("uncertain fence is not retried", func(t *testing.T) {
		s, store, turns, _, _, clock, _ := schedulerFixture(t)
		clock.Add(10 * time.Hour)
		store.addTask(cronTask("uncertain"))
		store.crossErr = errors.New("commit result lost")
		store.crossCommitError = true
		require.NoError(t, s.RunOnce(context.Background()))
		waitSchedulerIdle(t, s)
		run := runForTask(t, store, "uncertain")
		require.Equal(t, model.ScheduledRunInterrupted, run.Status)
		require.Equal(t, model.ScheduledReasonResultUnknown, run.Reason)
		require.Nil(t, run.NextAttemptAt)
		require.Len(t, turns.requests, 1)
		require.NoError(t, s.RunOnce(context.Background()))
		waitSchedulerIdle(t, s)
		require.Len(t, turns.requests, 1, "an uncertain start is never resubmitted")
	})
}

func TestSchedulerCapacityWaitsWithoutDroppingPending(t *testing.T) {
	s, store, turns, _, _, clock, _ := schedulerFixture(t)
	s.cfg.MaxConcurrent = 1
	s.sem = make(chan struct{}, 1)
	clock.Add(10 * time.Hour)
	store.addTask(cronTask("capacity-1"))
	store.addTask(cronTask("capacity-2"))
	turns.block = true

	require.NoError(t, s.RunOnce(context.Background()))
	require.Eventually(t, func() bool {
		turns.mu.Lock()
		defer turns.mu.Unlock()
		return len(turns.requests) == 1
	}, time.Second, time.Millisecond)
	close(turns.release)
	waitSchedulerIdle(t, s)
	require.NoError(t, s.RunOnce(context.Background()))
	waitSchedulerIdle(t, s)
	require.Len(t, turns.requests, 2)
	require.Equal(t, model.ScheduledRunSucceeded, runForTask(t, store, "capacity-1").Status)
	require.Equal(t, model.ScheduledRunSucceeded, runForTask(t, store, "capacity-2").Status)
}

func TestSchedulerTimeoutCancelsFromFence(t *testing.T) {
	s, store, turns, _, _, clock, _ := schedulerFixture(t)
	s.cfg.MaxRunDuration = 20 * time.Millisecond
	clock.Add(10 * time.Hour)
	store.addTask(cronTask("timeout"))
	turns.block = true
	require.NoError(t, s.RunOnce(context.Background()))
	waitSchedulerIdle(t, s)
	run := runForTask(t, store, "timeout")
	require.Equal(t, model.ScheduledRunCancelled, run.Status)
	require.Equal(t, model.ScheduledReasonTaskTimeout, run.Reason)
	require.NotNil(t, run.StartedAt)
	require.True(t, run.StartedAt.Equal(clock.Now()), "the timeout clock starts at the fence")
}

func TestSchedulerRecoveryDoesNotDuplicateExecution(t *testing.T) {
	t.Run("dead unknown becomes interrupted", func(t *testing.T) {
		s, store, turns, _, _, clock, manager := schedulerFixture(t)
		clock.Add(10 * time.Hour)
		store.addTask(cronTask("dead"))
		runs, err := store.MaterializeScheduledTasks(context.Background(), clock.Now(), 10)
		require.NoError(t, err)
		candidate := runs[0]
		store.mu.Lock()
		store.runs[candidate.OccurrenceID].Status = model.ScheduledRunRunning
		store.runs[candidate.OccurrenceID].UpdateTime = clock.Now().Add(-time.Hour)
		store.mu.Unlock()
		_, _, _ = manager.Store.GetMeta(context.Background(), candidate.RunID)
		require.NoError(t, s.RunOnce(context.Background()))
		waitSchedulerIdle(t, s)
		require.Equal(t, model.ScheduledRunInterrupted, runForTask(t, store, "dead").Status)
		require.Empty(t, turns.requests)
	})

	t.Run("terminal evidence is backfilled", func(t *testing.T) {
		s, store, turns, _, _, clock, manager := schedulerFixture(t)
		clock.Add(10 * time.Hour)
		store.addTask(cronTask("terminal"))
		runs, err := store.MaterializeScheduledTasks(context.Background(), clock.Now(), 10)
		require.NoError(t, err)
		candidate := runs[0]
		store.mu.Lock()
		store.runs[candidate.OccurrenceID].Status = model.ScheduledRunRunning
		store.mu.Unlock()
		require.NoError(t, manager.Store.InitMeta(context.Background(), candidate.RunID, run.RunMeta{SessionID: candidate.SessionID, UserID: candidate.UserID, Status: run.StatusDone}))
		require.NoError(t, s.RunOnce(context.Background()))
		waitSchedulerIdle(t, s)
		require.Equal(t, model.ScheduledRunSucceeded, runForTask(t, store, "terminal").Status)
		require.Empty(t, turns.requests)
	})

	t.Run("alive run is tracked then completed", func(t *testing.T) {
		s, store, turns, _, _, clock, manager := schedulerFixture(t)
		clock.Add(10 * time.Hour)
		store.addTask(cronTask("alive"))
		runs, err := store.MaterializeScheduledTasks(context.Background(), clock.Now(), 10)
		require.NoError(t, err)
		candidate := runs[0]
		store.mu.Lock()
		store.runs[candidate.OccurrenceID].Status = model.ScheduledRunRunning
		store.mu.Unlock()
		require.NoError(t, manager.Store.InitMeta(context.Background(), candidate.RunID, run.RunMeta{SessionID: candidate.SessionID, UserID: candidate.UserID, Status: run.StatusRunning}))
		require.NoError(t, manager.Store.Heartbeat(context.Background(), candidate.RunID, candidate.SessionID))
		require.NoError(t, s.RunOnce(context.Background()))
		require.NoError(t, manager.Store.SetStatus(context.Background(), candidate.RunID, run.StatusDone))
		waitSchedulerIdle(t, s)
		require.Equal(t, model.ScheduledRunSucceeded, runForTask(t, store, "alive").Status)
		require.Empty(t, turns.requests)
	})
}

func TestSchedulerTerminalWriteFailureIsRecoveredWithoutReplay(t *testing.T) {
	s, store, turns, _, _, clock, manager := schedulerFixture(t)
	s.cfg.RetryBackoff = time.Millisecond
	clock.Add(10 * time.Hour)
	store.addTask(cronTask("terminal-retry"))
	runs, err := store.MaterializeScheduledTasks(context.Background(), clock.Now(), 10)
	require.NoError(t, err)
	candidate := runs[0]
	store.mu.Lock()
	store.runs[candidate.OccurrenceID].Status = model.ScheduledRunRunning
	store.finishErrs = 3 // exhaust the callback's bounded attempts
	store.mu.Unlock()
	require.NoError(t, manager.Store.InitMeta(context.Background(), candidate.RunID, run.RunMeta{SessionID: candidate.SessionID, UserID: candidate.UserID, Status: run.StatusDone}))

	require.NoError(t, s.RunOnce(context.Background()))
	waitSchedulerIdle(t, s)
	require.Equal(t, model.ScheduledRunRunning, runForTask(t, store, "terminal-retry").Status)
	require.NoError(t, s.RunOnce(context.Background()))
	waitSchedulerIdle(t, s)
	require.Equal(t, model.ScheduledRunSucceeded, runForTask(t, store, "terminal-retry").Status)
	require.Empty(t, turns.requests, "terminal backfill never invokes the Agent again")
}

func TestSchedulerShutdownCancelsStartedAndPreservesPending(t *testing.T) {
	s, store, turns, _, _, clock, _ := schedulerFixture(t)
	s.cfg.MaxConcurrent = 1
	s.sem = make(chan struct{}, 1)
	clock.Add(10 * time.Hour)
	store.addTask(cronTask("shutdown-running"))
	store.addTask(cronTask("shutdown-pending"))
	turns.block = true
	require.NoError(t, s.RunOnce(context.Background()))
	require.Eventually(t, func() bool {
		turns.mu.Lock()
		defer turns.mu.Unlock()
		return len(turns.requests) == 1
	}, time.Second, time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, s.Shutdown(ctx))

	statuses := map[string]string{}
	cancelled, pending := 0, 0
	for _, run := range store.runs {
		statuses[run.TaskID] = run.Status
		switch run.Status {
		case model.ScheduledRunCancelled:
			cancelled++
		case model.ScheduledRunPending:
			pending++
		}
	}
	require.Equal(t, 1, cancelled, "the already-started run is cancelled by shutdown")
	require.Equal(t, 1, pending, "the unstarted occurrence remains durable for restart")
	require.NoError(t, s.RunOnce(context.Background()))
	require.Len(t, turns.requests, 1, "shutdown gate prevents every later submission")
}

func TestSchedulerCredentialsRemainOnlyInMemory(t *testing.T) {
	core, observed := observer.New(zap.DebugLevel)
	previousLogger := logger.L()
	logger.SetDefault(zap.New(core))
	t.Cleanup(func() { logger.SetDefault(previousLogger) })
	s, store, turns, _, tokens, clock, _ := schedulerFixture(t)
	clock.Add(10 * time.Hour)
	store.addTask(cronTask("secret"))
	require.NoError(t, s.RunOnce(context.Background()))
	waitSchedulerIdle(t, s)
	run := runForTask(t, store, "secret")
	require.Equal(t, model.ScheduledRunSucceeded, run.Status)
	require.NotContains(t, run.SnapshotJSON, "sk-runtime-secret")
	require.NotContains(t, run.LastError, "sk-runtime-secret")
	require.NotContains(t, run.Reason, "sk-runtime-secret")
	require.Len(t, tokens.users, 1)
	require.Equal(t, "user-1", tokens.users[0])
	require.Equal(t, "sk-runtime-secret", turns.requests[0].RawToken, "the token exists only in the in-memory request")
	require.Equal(t, "user-1", turns.userIDs[0], "per-user LLM resolution receives the owner context")
	serialized, err := json.Marshal(run)
	require.NoError(t, err)
	require.NotContains(t, string(serialized), "sk-runtime-secret")
	for _, entry := range observed.All() {
		require.NotContains(t, fmt.Sprint(entry), "sk-runtime-secret", "scheduler logs must not contain the execution credential")
	}
}
