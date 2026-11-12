package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/rpcx"
	"github.com/lush/blowball/internal/schedule"
)

type fakeStore struct {
	tasks    map[string]*model.ScheduledTask
	runs     map[string][]model.ScheduledTaskRun
	updates  int
	lastRev  int64
	lastTask *model.ScheduledTask
}

func newFakeStore() *fakeStore {
	return &fakeStore{tasks: map[string]*model.ScheduledTask{}, runs: map[string][]model.ScheduledTaskRun{}}
}

func (f *fakeStore) CreateTask(_ context.Context, t *model.ScheduledTask) error {
	c := *t
	f.tasks[t.TaskID] = &c
	return nil
}

func (f *fakeStore) GetTask(_ context.Context, userID, taskID string) (*model.ScheduledTask, error) {
	t, ok := f.tasks[taskID]
	if !ok || t.UserID != userID || t.DeletedAt != nil {
		return nil, nil
	}
	c := *t
	return &c, nil
}

func (f *fakeStore) ListTasks(_ context.Context, userID string, _, _ int) ([]model.ScheduledTask, error) {
	var out []model.ScheduledTask
	for _, t := range f.tasks {
		if t.UserID == userID && t.DeletedAt == nil {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdateTask(_ context.Context, t *model.ScheduledTask, expected int64) (int64, error) {
	if current, ok := f.tasks[t.TaskID]; ok && expected > 0 && current.Revision != expected {
		return 0, ErrRevisionConflict
	}
	f.updates++
	f.lastRev = expected
	c := *t
	f.lastTask = &c
	f.tasks[t.TaskID] = &c
	return t.Revision + 1, nil
}

func (f *fakeStore) DeleteTask(_ context.Context, userID, taskID string, now time.Time) (bool, error) {
	t, ok := f.tasks[taskID]
	if !ok || t.UserID != userID || t.DeletedAt != nil {
		return false, nil
	}
	t.DeletedAt = &now
	return true, nil
}

func (f *fakeStore) ListOccurrences(_ context.Context, userID, taskID string, _, _ int) ([]model.ScheduledTaskRun, error) {
	var out []model.ScheduledTaskRun
	for _, r := range f.runs[taskID] {
		if r.UserID == userID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) GetOccurrence(_ context.Context, occurrenceID string) (*model.ScheduledTaskRun, error) {
	for _, runs := range f.runs {
		for i := range runs {
			if runs[i].OccurrenceID == occurrenceID {
				return &runs[i], nil
			}
		}
	}
	return nil, nil
}

func (f *fakeStore) ResourceReferences(context.Context, string, uint64) (bool, error) {
	return false, nil
}

type fakeValidator struct {
	err    error
	model  string
	effort string
	calls  int
	last   ValidateConfig
}

func (f *fakeValidator) ValidateTaskConfig(_ context.Context, _, _ string, cfg ValidateConfig) (string, string, error) {
	f.calls++
	f.last = cfg
	if f.err != nil {
		return "", "", f.err
	}
	m, e := f.model, f.effort
	if m == "" {
		m = "model-a"
	}
	if e == "" {
		e = "none"
	}
	return m, e, nil
}

func testClock() func() time.Time {
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return base }
}

func newService(t *testing.T) (*Service, *fakeStore, *fakeValidator) {
	t.Helper()
	store := newFakeStore()
	validator := &fakeValidator{}
	return New(store, validator, 5000, testClock()), store, validator
}

func TestCreateValidatesAndPersists(t *testing.T) {
	svc, store, validator := newService(t)
	created, err := svc.Create(context.Background(), CreateInput{
		ActorUserID: "user-1", Name: "nightly", Prompt: "summarize",
		Schedule: schedule.Rule{Type: schedule.TypeCron, Expression: "0 2 * * *", Timezone: "Asia/Shanghai"},
		Model:    "model-a",
	})
	require.NoError(t, err)
	assert.Equal(t, "user-1", created.UserID)
	assert.Equal(t, int64(1), created.Revision)
	assert.Equal(t, model.ScheduledSessionNew, created.SessionMode)
	assert.Equal(t, "model-a", created.Model)
	assert.NotNil(t, created.NextRunAt)
	assert.Equal(t, 1, validator.calls)
	require.Contains(t, store.tasks, created.TaskID)
}

func TestCreateRejectsInvalidInputs(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	cases := []struct {
		name string
		in   CreateInput
		code string
	}{
		{"empty name", CreateInput{ActorUserID: "u", Prompt: "p", Schedule: cronRule()}, "INVALID_NAME"},
		{"empty prompt", CreateInput{ActorUserID: "u", Name: "n", Schedule: cronRule()}, "INVALID_PROMPT"},
		{"past at", CreateInput{ActorUserID: "u", Name: "n", Prompt: "p",
			Schedule: schedule.Rule{Type: schedule.TypeAt, RunAt: "2020-01-01T00:00:00Z", Timezone: "UTC"}}, "INVALID_SCHEDULE"},
		{"bad timezone", CreateInput{ActorUserID: "u", Name: "n", Prompt: "p",
			Schedule: schedule.Rule{Type: schedule.TypeCron, Expression: "* * * * *", Timezone: "Not/AZone"}}, "INVALID_SCHEDULE"},
		{"resource pair", CreateInput{ActorUserID: "u", Name: "n", Prompt: "p", Schedule: cronRule(),
			ResourceRevision: 2}, "INVALID_RESOURCE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Create(ctx, tc.in)
			require.Error(t, err)
			var ve *ValidationError
			require.True(t, errors.As(err, &ve))
			assert.Equal(t, tc.code, ve.Code)
		})
	}
}

func TestUpdateRevisionConflict(t *testing.T) {
	svc, store, _ := newService(t)
	created, err := svc.Create(context.Background(), CreateInput{
		ActorUserID: "user-1", Name: "n", Prompt: "p", Schedule: cronRule(),
	})
	require.NoError(t, err)
	store.tasks[created.TaskID].Revision = 7
	_, err = svc.Update(context.Background(), UpdateInput{
		ActorUserID: "user-1", TaskID: created.TaskID, ExpectedRevision: 3, Name: ptr("new"),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRevisionConflict)
	assert.Equal(t, 0, store.updates)
}

func TestUpdateMigratesLegacySessionPolicy(t *testing.T) {
	svc, store, _ := newService(t)
	sessionID := "legacy-session"
	store.tasks["task-1"] = &model.ScheduledTask{
		TaskID: "task-1", UserID: "user-1", Name: "n", Prompt: "p",
		ScheduleType: schedule.TypeCron, CronExpression: "0 2 * * *", Timezone: "UTC",
		SessionMode: model.ScheduledSessionExisting, SessionID: &sessionID,
		Enabled: true, Revision: 2,
	}
	updated, err := svc.Update(context.Background(), UpdateInput{
		ActorUserID: "user-1", TaskID: "task-1", ExpectedRevision: 2, Name: ptr("renamed"),
	})
	require.NoError(t, err)
	assert.Equal(t, model.ScheduledSessionNew, updated.SessionMode)
	assert.Nil(t, updated.SessionID)
}

func TestPauseResumeFutureAtKeepsSlot(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := t.Context()
	future := svc.now().Add(2 * time.Hour)
	created, err := svc.Create(ctx, CreateInput{
		ActorUserID: "user-1", Name: "n", Prompt: "p",
		Schedule: schedule.Rule{Type: schedule.TypeAt, RunAt: future.Format(time.RFC3339), Timezone: "UTC"},
	})
	require.NoError(t, err)
	assert.Equal(t, new(future), created.NextRunAt)

	paused, err := svc.Update(ctx, UpdateInput{
		ActorUserID: "user-1", TaskID: created.TaskID, Enabled: new(false),
	})
	require.NoError(t, err)
	assert.False(t, paused.Enabled)
	assert.Equal(t, model.ScheduledReasonTaskPaused, paused.PausedReason)
	assert.Equal(t, new(future), paused.NextRunAt)

	resumed, err := svc.Update(ctx, UpdateInput{
		ActorUserID: "user-1", TaskID: created.TaskID, Enabled: new(true),
	})
	require.NoError(t, err)
	assert.True(t, resumed.Enabled)
	assert.Empty(t, resumed.PausedReason)
	assert.Equal(t, new(future), resumed.NextRunAt, "enabled=%t at task lost its future slot", resumed.Enabled)
}

func TestEnableInitiallyDisabledFutureAtKeepsSlot(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := t.Context()
	future := svc.now().Add(2 * time.Hour)
	created, err := svc.Create(ctx, CreateInput{
		ActorUserID: "user-1", Name: "n", Prompt: "p",
		Schedule: schedule.Rule{Type: schedule.TypeAt, RunAt: future.Format(time.RFC3339), Timezone: "UTC"},
		Enabled:  new(false),
	})
	require.NoError(t, err)
	assert.False(t, created.Enabled)
	assert.Equal(t, model.ScheduledReasonTaskPaused, created.PausedReason)
	assert.Equal(t, new(future), created.NextRunAt)

	resumed, err := svc.Update(ctx, UpdateInput{
		ActorUserID: "user-1", TaskID: created.TaskID, Enabled: new(true),
	})
	require.NoError(t, err)
	assert.True(t, resumed.Enabled)
	assert.Empty(t, resumed.PausedReason)
	assert.Equal(t, new(future), resumed.NextRunAt, "enabled=%t at task lost its future slot", resumed.Enabled)
}

func TestResumeConsumedAtDoesNotReplay(t *testing.T) {
	svc, store, _ := newService(t)
	store.tasks["task-at"] = &model.ScheduledTask{
		TaskID: "task-at", UserID: "user-1", Name: "n", Prompt: "p",
		ScheduleType: schedule.TypeAt, RunAt: timePtr(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)),
		Timezone: "UTC", SessionMode: model.ScheduledSessionNew, Enabled: false, Revision: 1,
	}
	// NextRunAt nil = the slot was already consumed. Enabling must not replay.
	updated, err := svc.Update(context.Background(), UpdateInput{
		ActorUserID: "user-1", TaskID: "task-at", Enabled: ptr(true),
	})
	require.NoError(t, err)
	assert.Nil(t, updated.NextRunAt)
}

func TestResumePausedCronSchedulesFutureSlot(t *testing.T) {
	svc, store, _ := newService(t)
	store.tasks["task-cron"] = &model.ScheduledTask{
		TaskID: "task-cron", UserID: "user-1", Name: "n", Prompt: "p",
		ScheduleType: schedule.TypeCron, CronExpression: "0 2 * * *", Timezone: "UTC",
		SessionMode: model.ScheduledSessionNew, Enabled: false, Revision: 1,
		PausedReason: model.ScheduledReasonTaskPaused,
	}
	updated, err := svc.Update(context.Background(), UpdateInput{
		ActorUserID: "user-1", TaskID: "task-cron", Enabled: ptr(true),
	})
	require.NoError(t, err)
	require.NotNil(t, updated.NextRunAt)
	assert.True(t, updated.NextRunAt.After(testClock()()))
}

func TestCreateFailsClosedWhenControlIsUnavailable(t *testing.T) {
	svc, _, validator := newService(t)
	validator.err = rpcx.Errorf(rpcx.CodeUnavailable, "main service offline")
	_, err := svc.Create(context.Background(), CreateInput{
		ActorUserID: "user-1", Name: "n", Prompt: "p", Schedule: cronRule(),
	})
	require.Error(t, err)
	de, ok := rpcx.AsError(err)
	require.True(t, ok)
	assert.Equal(t, rpcx.CodeUnavailable, de.Code)
}

func TestOccurrencesAreReadableAfterTaskDeletion(t *testing.T) {
	svc, store, _ := newService(t)
	store.runs["task-1"] = []model.ScheduledTaskRun{{OccurrenceID: "occ-1", TaskID: "task-1", UserID: "user-1"}}
	store.tasks["task-1"] = &model.ScheduledTask{TaskID: "task-1", UserID: "user-1", DeletedAt: timePtr(testClock()())}
	runs, err := svc.Occurrences(context.Background(), "user-1", "task-1", 20, 0)
	require.NoError(t, err)
	assert.Len(t, runs, 1)

	foreign, err := svc.Occurrences(context.Background(), "user-2", "task-1", 20, 0)
	require.NoError(t, err)
	assert.Empty(t, foreign)
}

func cronRule() schedule.Rule {
	return schedule.Rule{Type: schedule.TypeCron, Expression: "0 2 * * *", Timezone: "UTC"}
}

func ptr[T any](v T) *T { return &v }
