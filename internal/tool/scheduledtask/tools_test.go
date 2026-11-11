package scheduledtask

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/resource"
	"github.com/lush/blowball/internal/rpcx"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/skill"
)

type fakeScheduler struct {
	createReq *schedulerv1.CreateTaskRequest
	updateReq *schedulerv1.UpdateTaskRequest
	created   *schedulerv1.Task
	updated   *schedulerv1.Task
	err       error
}

func (f *fakeScheduler) CreateTask(_ context.Context, req *schedulerv1.CreateTaskRequest) (*schedulerv1.CreateTaskResponse, error) {
	f.createReq = req
	return &schedulerv1.CreateTaskResponse{Task: f.created}, f.err
}

func (f *fakeScheduler) GetTask(_ context.Context, req *schedulerv1.GetTaskRequest) (*schedulerv1.GetTaskResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	if req.GetTaskId() == "foreign" {
		return nil, rpcx.Errorf(rpcx.CodeNotFound, "missing")
	}
	return &schedulerv1.GetTaskResponse{Task: f.created}, nil
}

func (f *fakeScheduler) ListTasks(context.Context, *schedulerv1.ListTasksRequest) (*schedulerv1.ListTasksResponse, error) {
	return &schedulerv1.ListTasksResponse{Tasks: []*schedulerv1.Task{f.created}}, f.err
}

func (f *fakeScheduler) UpdateTask(_ context.Context, req *schedulerv1.UpdateTaskRequest) (*schedulerv1.UpdateTaskResponse, error) {
	f.updateReq = req
	return &schedulerv1.UpdateTaskResponse{Task: f.updated}, f.err
}

func (f *fakeScheduler) DeleteTask(context.Context, *schedulerv1.DeleteTaskRequest) (*schedulerv1.DeleteTaskResponse, error) {
	return &schedulerv1.DeleteTaskResponse{}, f.err
}

type fakeResources struct{}

func (fakeResources) GetDraft(context.Context, string, string) (*resource.DraftInfo, error) {
	return &resource.DraftInfo{TaskID: "task-1", DraftRevision: 2}, nil
}
func (fakeResources) ListVersions(context.Context, string, string) ([]model.ScheduledResourceVersion, error) {
	return nil, nil
}
func (fakeResources) Publish(context.Context, string, string, uint64, string) (*model.ScheduledResourceVersion, error) {
	return &model.ScheduledResourceVersion{Revision: 3, ManifestHash: "h"}, nil
}

func toolFixture(t *testing.T) (*tool.Registry, *fakeScheduler) {
	t.Helper()
	sched := &fakeScheduler{created: &schedulerv1.Task{
		TaskId: "task-1", Revision: 1,
		Config: &schedulerv1.TaskConfig{Name: "n", Prompt: "p", Enabled: true},
	}}
	sched.updated = sched.created
	reg := tool.NewRegistry()
	require.NoError(t, RegisterAll(reg, NewTools(sched, fakeResources{}, "model-default")))
	return reg, sched
}

func execCtx(userID string) context.Context {
	return skill.WithUserID(context.Background(), userID)
}

func TestCreateUsesTrustedActorAndNoUserField(t *testing.T) {
	reg, sched := toolFixture(t)
	spec, ok := reg.Get(NameCreate)
	require.True(t, ok)
	assert.NotContains(t, string(spec.ParametersJSON), "user_id", "the tool must not accept a forged actor")
	assert.NotContains(t, string(spec.ParametersJSON), "user")
	_, err := spec.Execute(execCtx("user-1"), json.RawMessage(`{
      "name":"n","prompt":"p","schedule":{"type":"cron","expression":"0 2 * * *","timezone":"UTC"}}`))
	require.NoError(t, err)
	require.NotNil(t, sched.createReq)
	assert.Equal(t, "user-1", sched.createReq.GetActor().GetUserId())
}

func TestToolRequiresAuthenticatedContext(t *testing.T) {
	reg, _ := toolFixture(t)
	spec, _ := reg.Get(NameList)
	_, err := spec.Execute(context.Background(), json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authenticated")
}

func TestCrossUserTaskIsNotFound(t *testing.T) {
	reg, _ := toolFixture(t)
	spec, _ := reg.Get(NameGet)
	_, err := spec.Execute(execCtx("user-1"), json.RawMessage(`{"task_id":"foreign"}`))
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "not found"))
}

func TestPauseAndResumeSetEnabled(t *testing.T) {
	reg, sched := toolFixture(t)
	pause, _ := reg.Get(NamePause)
	_, err := pause.Execute(execCtx("user-1"), json.RawMessage(`{"task_id":"task-1"}`))
	require.NoError(t, err)
	require.NotNil(t, sched.updateReq)
	assert.False(t, sched.updateReq.GetConfig().GetEnabled())

	resume, _ := reg.Get(NameResume)
	_, err = resume.Execute(execCtx("user-1"), json.RawMessage(`{"task_id":"task-1"}`))
	require.NoError(t, err)
	assert.True(t, sched.updateReq.GetConfig().GetEnabled())
}

func TestResourcesTools(t *testing.T) {
	reg, _ := toolFixture(t)
	get, _ := reg.Get(NameResourcesGet)
	res, err := get.Execute(execCtx("user-1"), json.RawMessage(`{"task_id":"task-1"}`))
	require.NoError(t, err)
	m, ok := res.(map[string]any)
	require.True(t, ok)
	assert.Contains(t, m, "draft")

	publish, _ := reg.Get(NameResourcesPublish)
	res, err = publish.Execute(execCtx("user-1"), json.RawMessage(`{"task_id":"task-1","request_id":"req-1"}`))
	require.NoError(t, err)
	m, ok = res.(map[string]any)
	require.True(t, ok)
	assert.Contains(t, m, "resource_revision")
}

func TestRegisterAllRequiresScheduler(t *testing.T) {
	require.Error(t, RegisterAll(tool.NewRegistry(), NewTools(nil, nil, "")))
}

func TestToolErrorsAreClear(t *testing.T) {
	sched := &fakeScheduler{err: rpcx.Errorf(rpcx.CodeUnavailable, "down")}
	reg := tool.NewRegistry()
	require.NoError(t, RegisterAll(reg, NewTools(sched, nil, "")))
	spec, _ := reg.Get(NameList)
	_, err := spec.Execute(execCtx("user-1"), json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unavailable")
}
