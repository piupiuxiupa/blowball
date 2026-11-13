package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/schedule"
)

func TestScheduledTaskDefaultsAndSnapshotContainNoCredentials(t *testing.T) {
	task := &ScheduledTask{
		TaskID: "task-1", UserID: "user-1", Name: "report", Prompt: "summarize",
		Model: "", ReasoningEffort: "",
	}
	task.SetSchedule(schedule.Rule{Type: schedule.TypeCron, Expression: "0 9 * * *", Timezone: "UTC"})
	task.ApplyDefaults()
	require.Equal(t, ScheduledSessionNew, task.Session.Mode)
	require.Equal(t, int64(1), task.Revision)

	snapshot := task.Snapshot()
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "jwt")
	require.NotContains(t, string(raw), "llm_token")
	require.NotContains(t, string(raw), "api_key")
	require.NotContains(t, string(raw), "sk-super-secret")
	require.Equal(t, ScheduledSessionNew, snapshot.Session.Mode)

	run := &ScheduledTaskRun{SnapshotJSON: string(raw), ClaimToken: "lease-only"}
	decoded, err := run.Snapshot()
	require.NoError(t, err)
	require.Equal(t, snapshot, decoded)
	apiJSON, err := json.Marshal(run)
	require.NoError(t, err)
	require.NotContains(t, string(apiJSON), "snapshot_json")
	require.NotContains(t, string(apiJSON), "claim_token")
}

func TestScheduledTaskSetScheduleNormalizesAtToUTC(t *testing.T) {
	task := &ScheduledTask{}
	task.SetSchedule(schedule.Rule{Type: schedule.TypeAt, RunAt: "2026-11-05T09:30:00+08:00", Timezone: "Asia/Shanghai"})
	require.NotNil(t, task.RunAt)
	require.Equal(t, time.Date(2026, 11, 5, 1, 30, 0, 0, time.UTC), *task.RunAt)
	require.Equal(t, "2026-11-05T01:30:00Z", task.Rule().RunAt)
}

func TestScheduledRunTerminal(t *testing.T) {
	for _, status := range []string{ScheduledRunSucceeded, ScheduledRunError, ScheduledRunSkipped, ScheduledRunCancelled, ScheduledRunInterrupted} {
		require.True(t, ScheduledRunTerminal(status), status)
	}
	for _, status := range []string{ScheduledRunPending, ScheduledRunPreparing, ScheduledRunLaunching, ScheduledRunRunning} {
		require.False(t, ScheduledRunTerminal(status), status)
	}
}
