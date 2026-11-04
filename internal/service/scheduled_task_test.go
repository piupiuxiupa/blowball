package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/modelselect"
	"github.com/lush/blowball/internal/schedule"
)

type fakeScheduledTaskStore struct {
	tasks    map[string]*model.ScheduledTask
	runs     map[string][]model.ScheduledTaskRun
	sessions map[string]model.Session
}

func newFakeScheduledTaskStore() *fakeScheduledTaskStore {
	return &fakeScheduledTaskStore{tasks: map[string]*model.ScheduledTask{}, runs: map[string][]model.ScheduledTaskRun{}, sessions: map[string]model.Session{}}
}

func cloneScheduledTask(task *model.ScheduledTask) *model.ScheduledTask {
	if task == nil {
		return nil
	}
	cp := *task
	if task.RunAt != nil {
		v := *task.RunAt
		cp.RunAt = &v
	}
	if task.NextRunAt != nil {
		v := *task.NextRunAt
		cp.NextRunAt = &v
	}
	if task.SessionID != nil {
		v := *task.SessionID
		cp.SessionID = &v
	}
	if task.DeletedAt != nil {
		v := *task.DeletedAt
		cp.DeletedAt = &v
	}
	return &cp
}

func (f *fakeScheduledTaskStore) CreateScheduledTask(_ context.Context, task *model.ScheduledTask) error {
	f.tasks[task.TaskID] = cloneScheduledTask(task)
	return nil
}

func (f *fakeScheduledTaskStore) GetScheduledTask(_ context.Context, userID, taskID string) (*model.ScheduledTask, error) {
	task := f.tasks[taskID]
	if task == nil || task.UserID != userID || task.DeletedAt != nil {
		return nil, nil
	}
	return cloneScheduledTask(task), nil
}

func (f *fakeScheduledTaskStore) ListScheduledTasks(_ context.Context, userID string, limit, offset int) ([]model.ScheduledTask, error) {
	var out []model.ScheduledTask
	for _, task := range f.tasks {
		if task.UserID == userID && task.DeletedAt == nil {
			out = append(out, *cloneScheduledTask(task))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreateTime.Equal(out[j].CreateTime) {
			return out[i].TaskID > out[j].TaskID
		}
		return out[i].CreateTime.After(out[j].CreateTime)
	})
	return scheduledTaskSlice(out, limit, offset), nil
}

func (f *fakeScheduledTaskStore) UpdateScheduledTask(_ context.Context, task *model.ScheduledTask) (bool, error) {
	current := f.tasks[task.TaskID]
	if current == nil || current.UserID != task.UserID || current.DeletedAt != nil {
		return false, nil
	}
	cp := cloneScheduledTask(task)
	cp.Revision = current.Revision + 1
	f.tasks[task.TaskID] = cp
	if !cp.Enabled {
		for i := range f.runs[cp.TaskID] {
			run := &f.runs[cp.TaskID][i]
			if run.StartedAt == nil && (run.Status == model.ScheduledRunPending || run.Status == model.ScheduledRunPreparing || run.Status == model.ScheduledRunLaunching) {
				run.Status = model.ScheduledRunCancelled
				run.Reason = model.ScheduledReasonTaskPaused
			}
		}
	}
	return true, nil
}

func (f *fakeScheduledTaskStore) DeleteScheduledTask(_ context.Context, userID, taskID string, now time.Time) (bool, error) {
	task := f.tasks[taskID]
	if task == nil || task.UserID != userID || task.DeletedAt != nil {
		return false, nil
	}
	task.DeletedAt = &now
	task.Enabled = false
	return true, nil
}

func (f *fakeScheduledTaskStore) ListScheduledTaskRuns(_ context.Context, userID, taskID string, limit, offset int) ([]model.ScheduledTaskRun, error) {
	runs := append([]model.ScheduledTaskRun(nil), f.runs[taskID]...)
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].ScheduledFor.Equal(runs[j].ScheduledFor) {
			return runs[i].ID > runs[j].ID
		}
		return runs[i].ScheduledFor.After(runs[j].ScheduledFor)
	})
	var filtered []model.ScheduledTaskRun
	for _, run := range runs {
		if run.UserID == userID {
			filtered = append(filtered, run)
		}
	}
	return scheduledTaskRunSlice(filtered, limit, offset), nil
}

func (f *fakeScheduledTaskStore) GetSessionByID(_ context.Context, sessionID string) (*model.Session, error) {
	sess, ok := f.sessions[sessionID]
	if !ok {
		return nil, nil
	}
	return &sess, nil
}

func scheduledTaskSlice[T any](values []T, limit, offset int) []T {
	if offset > len(values) {
		offset = len(values)
	}
	values = values[offset:]
	if limit > len(values) {
		limit = len(values)
	}
	return values[:limit]
}

func scheduledTaskRunSlice(values []model.ScheduledTaskRun, limit, offset int) []model.ScheduledTaskRun {
	return scheduledTaskSlice(values, limit, offset)
}

func scheduledServiceFixture(t *testing.T) (*ScheduledTaskService, *fakeScheduledTaskStore, *time.Time) {
	t.Helper()
	store := newFakeScheduledTaskStore()
	store.sessions["session-owner"] = model.Session{SessionID: "session-owner", UserID: "user-1"}
	store.sessions["session-other"] = model.Session{SessionID: "session-other", UserID: "user-2"}
	selection := modelselect.Config{
		Catalog: []config.ModelCatalogEntry{
			{Name: "thinking", Thinking: true, MaxContextTokens: 10000, MaxCompletionTokens: 1000},
			{Name: "plain", Thinking: false, MaxContextTokens: 10000, MaxCompletionTokens: 1000},
		},
		Default: "thinking", DefaultEffort: "none",
	}
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	return NewScheduledTaskService(store, selection, 100, func() time.Time { return now }), store, &now
}

func createInput() CreateScheduledTaskInput {
	return CreateScheduledTaskInput{
		Name: "daily", Prompt: "write a report",
		Schedule: schedule.Rule{Type: schedule.TypeCron, Expression: "30 9 * * *", Timezone: "UTC"},
	}
}

func TestScheduledTaskServiceCreateValidationAndDefaults(t *testing.T) {
	svc, store, _ := scheduledServiceFixture(t)
	ctx := context.Background()
	created, err := svc.Create(ctx, "user-1", createInput())
	require.NoError(t, err)
	require.NotEmpty(t, created.TaskID)
	require.True(t, created.Enabled)
	require.Equal(t, model.ScheduledSessionNew, created.Session.Mode)
	require.Equal(t, time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC), *created.NextRunAt)
	require.Len(t, store.tasks, 1)

	existing := createInput()
	existing.Session = model.ScheduledSessionPolicy{Mode: model.ScheduledSessionExisting, SessionID: "session-owner"}
	createdExisting, err := svc.Create(ctx, "user-1", existing)
	require.NoError(t, err)
	require.Equal(t, "session-owner", createdExisting.Session.SessionID)

	for name, mutate := range map[string]func(*CreateScheduledTaskInput){
		"blank name":   func(i *CreateScheduledTaskInput) { i.Name = " " },
		"blank prompt": func(i *CreateScheduledTaskInput) { i.Prompt = "" },
		"past at": func(i *CreateScheduledTaskInput) {
			i.Schedule = schedule.Rule{Type: schedule.TypeAt, RunAt: "2026-11-03T00:00:00Z", Timezone: "UTC"}
		},
		"bad model":  func(i *CreateScheduledTaskInput) { i.Model = "missing" },
		"bad effort": func(i *CreateScheduledTaskInput) { i.Model = "plain"; i.ReasoningEffort = "high" },
		"foreign session": func(i *CreateScheduledTaskInput) {
			i.Session = model.ScheduledSessionPolicy{Mode: model.ScheduledSessionExisting, SessionID: "session-other"}
		},
		"new with session": func(i *CreateScheduledTaskInput) { i.Session.SessionID = "session-owner" },
	} {
		t.Run(name, func(t *testing.T) {
			input := createInput()
			mutate(&input)
			before := len(store.tasks)
			_, err := svc.Create(ctx, "user-1", input)
			require.Error(t, err)
			require.Len(t, store.tasks, before)
		})
	}
}

func TestScheduledTaskServiceTokenLimit(t *testing.T) {
	svc, _, _ := scheduledServiceFixture(t)
	svc.maxInputTokens = 2
	input := createInput()
	input.Prompt = "too many tokens"
	_, err := svc.Create(context.Background(), "user-1", input)
	var validation *ScheduledTaskError
	require.ErrorAs(t, err, &validation)
	require.Equal(t, "CONTENT_TOO_LONG", validation.Code)
}

func TestScheduledTaskServicePauseResumeCronAndAt(t *testing.T) {
	svc, store, now := scheduledServiceFixture(t)
	ctx := context.Background()
	cron, err := svc.Create(ctx, "user-1", createInput())
	require.NoError(t, err)
	originalNext := *cron.NextRunAt

	disabled := false
	cron, err = svc.Update(ctx, "user-1", cron.TaskID, UpdateScheduledTaskInput{Enabled: &disabled})
	require.NoError(t, err)
	require.False(t, cron.Enabled)
	require.Equal(t, originalNext, *cron.NextRunAt)
	store.runs[cron.TaskID] = []model.ScheduledTaskRun{{TaskID: cron.TaskID, UserID: "user-1", Status: model.ScheduledRunPending}}
	cron, err = svc.Update(ctx, "user-1", cron.TaskID, UpdateScheduledTaskInput{Enabled: &disabled})
	require.NoError(t, err)
	require.Equal(t, model.ScheduledRunCancelled, store.runs[cron.TaskID][0].Status)

	*now = now.Add(72 * time.Hour)
	enabled := true
	cron, err = svc.Update(ctx, "user-1", cron.TaskID, UpdateScheduledTaskInput{Enabled: &enabled})
	require.NoError(t, err)
	require.True(t, cron.Enabled)
	require.Equal(t, time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC), *cron.NextRunAt)
	require.NotEqual(t, originalNext, *cron.NextRunAt, "resume must not replay paused cron slots")

	atInput := createInput()
	atInput.Schedule = schedule.Rule{Type: schedule.TypeAt, RunAt: now.Add(time.Hour).Format(time.RFC3339), Timezone: "UTC"}
	atTask, err := svc.Create(ctx, "user-1", atInput)
	require.NoError(t, err)
	atTask, err = svc.Update(ctx, "user-1", atTask.TaskID, UpdateScheduledTaskInput{Enabled: &disabled})
	require.NoError(t, err)
	*now = now.Add(2 * time.Hour) // overdue but never materialized
	atTask, err = svc.Update(ctx, "user-1", atTask.TaskID, UpdateScheduledTaskInput{Enabled: &enabled})
	require.NoError(t, err)
	require.NotNil(t, atTask.NextRunAt)
	require.True(t, atTask.NextRunAt.Before(*now), "unconsumed at remains eligible for one catch-up")

	store.tasks[atTask.TaskID].NextRunAt = nil // consumed
	atTask, err = svc.Update(ctx, "user-1", atTask.TaskID, UpdateScheduledTaskInput{Enabled: &disabled})
	require.NoError(t, err)
	atTask, err = svc.Update(ctx, "user-1", atTask.TaskID, UpdateScheduledTaskInput{Enabled: &enabled})
	require.NoError(t, err)
	require.Nil(t, atTask.NextRunAt, "enabling a consumed at does not replay it")

	newRule := schedule.Rule{Type: schedule.TypeAt, RunAt: now.Add(24 * time.Hour).Format(time.RFC3339), Timezone: "UTC"}
	atTask, err = svc.Update(ctx, "user-1", atTask.TaskID, UpdateScheduledTaskInput{Schedule: &newRule})
	require.NoError(t, err)
	require.NotNil(t, atTask.NextRunAt)
	require.Equal(t, newRule.RunAt, atTask.NextRunAt.Format(time.RFC3339))
}

func TestScheduledTaskServiceInvalidUpdateLeavesStoredTaskUnchanged(t *testing.T) {
	svc, store, _ := scheduledServiceFixture(t)
	ctx := context.Background()
	created, err := svc.Create(ctx, "user-1", createInput())
	require.NoError(t, err)
	name := "new name"
	badEffort := "high"
	modelName := "plain"
	_, err = svc.Update(ctx, "user-1", created.TaskID, UpdateScheduledTaskInput{Name: &name, Model: &modelName, ReasoningEffort: &badEffort})
	require.Error(t, err)
	stored := store.tasks[created.TaskID]
	require.Equal(t, "daily", stored.Name)
	require.Equal(t, int64(1), stored.Revision)

	_, err = svc.Update(ctx, "user-2", created.TaskID, UpdateScheduledTaskInput{Name: &name})
	require.ErrorIs(t, err, ErrScheduledTaskNotFound)
	require.Equal(t, "daily", store.tasks[created.TaskID].Name)
}

func TestScheduledTaskServiceListRunsPaginationAndNotFound(t *testing.T) {
	svc, store, _ := scheduledServiceFixture(t)
	ctx := context.Background()
	created, err := svc.Create(ctx, "user-1", createInput())
	require.NoError(t, err)
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		store.runs[created.TaskID] = append(store.runs[created.TaskID], model.ScheduledTaskRun{
			ID: int64(i), OccurrenceID: string(rune('a' + i - 1)), TaskID: created.TaskID, UserID: "user-1", ScheduledFor: now.Add(time.Duration(i) * time.Minute),
		})
	}
	runs, err := svc.Runs(ctx, "user-1", created.TaskID, 2, 0)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	require.Equal(t, int64(3), runs[0].ID)
	runs, err = svc.Runs(ctx, "user-1", created.TaskID, 2, 2)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, int64(1), runs[0].ID)
	_, err = svc.Runs(ctx, "user-2", created.TaskID, 20, 0)
	require.ErrorIs(t, err, ErrScheduledTaskNotFound)

	require.NoError(t, svc.Delete(ctx, "user-1", created.TaskID))
	_, err = svc.Get(ctx, "user-1", created.TaskID)
	require.ErrorIs(t, err, ErrScheduledTaskNotFound)
	require.ErrorIs(t, svc.Delete(ctx, "user-1", created.TaskID), ErrScheduledTaskNotFound)
}

func TestScheduledTaskServiceValidationErrorType(t *testing.T) {
	svc, _, _ := scheduledServiceFixture(t)
	input := createInput()
	input.Name = strings.Repeat("x", 256)
	_, err := svc.Create(context.Background(), "user-1", input)
	var validation *ScheduledTaskError
	require.True(t, errors.As(err, &validation))
	require.Equal(t, "INVALID_NAME", validation.Code)
}
