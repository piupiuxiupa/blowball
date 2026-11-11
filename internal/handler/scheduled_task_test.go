package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/rpcx"
)

type fakeScheduledTaskRPC struct {
	task        *schedulerv1.Task
	err         error
	createCalls int
	createUser  string
	lastConfig  *schedulerv1.TaskConfig
	lastUpdate  *schedulerv1.UpdateTaskRequest
	listLimit   int32
	listOffset  int32
	runsLimit   int32
	runsOffset  int32
	deleteCalls int
}

func (f *fakeScheduledTaskRPC) CreateTask(_ context.Context, req *schedulerv1.CreateTaskRequest) (*schedulerv1.CreateTaskResponse, error) {
	f.createCalls++
	f.createUser = req.GetActor().GetUserId()
	f.lastConfig = req.GetConfig()
	return &schedulerv1.CreateTaskResponse{Task: f.task}, f.err
}

func (f *fakeScheduledTaskRPC) GetTask(context.Context, *schedulerv1.GetTaskRequest) (*schedulerv1.GetTaskResponse, error) {
	return &schedulerv1.GetTaskResponse{Task: f.task}, f.err
}

func (f *fakeScheduledTaskRPC) ListTasks(_ context.Context, req *schedulerv1.ListTasksRequest) (*schedulerv1.ListTasksResponse, error) {
	f.listLimit, f.listOffset = req.GetLimit(), req.GetOffset()
	if f.err != nil {
		return nil, f.err
	}
	if f.task != nil {
		return &schedulerv1.ListTasksResponse{Tasks: []*schedulerv1.Task{f.task}}, nil
	}
	return &schedulerv1.ListTasksResponse{}, nil
}

func (f *fakeScheduledTaskRPC) UpdateTask(_ context.Context, req *schedulerv1.UpdateTaskRequest) (*schedulerv1.UpdateTaskResponse, error) {
	f.lastUpdate = req
	return &schedulerv1.UpdateTaskResponse{Task: f.task}, f.err
}

func (f *fakeScheduledTaskRPC) DeleteTask(context.Context, *schedulerv1.DeleteTaskRequest) (*schedulerv1.DeleteTaskResponse, error) {
	f.deleteCalls++
	return &schedulerv1.DeleteTaskResponse{Task: f.task}, f.err
}

func (f *fakeScheduledTaskRPC) ListOccurrences(_ context.Context, req *schedulerv1.ListOccurrencesRequest) (*schedulerv1.ListOccurrencesResponse, error) {
	f.runsLimit, f.runsOffset = req.GetLimit(), req.GetOffset()
	return &schedulerv1.ListOccurrencesResponse{}, f.err
}

func newScheduledTaskHandlerTestEnv(rpc *fakeScheduledTaskRPC, authenticated bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewScheduledTaskHandler(rpc, "model-default")
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if authenticated {
			c.Set(middleware.UserIDKey, "user-1")
		}
		c.Next()
	})
	r.POST("/api/v1/scheduled-tasks", h.Create)
	r.GET("/api/v1/scheduled-tasks", h.List)
	r.GET("/api/v1/scheduled-tasks/:task_id", h.Get)
	r.PATCH("/api/v1/scheduled-tasks/:task_id", h.Update)
	r.DELETE("/api/v1/scheduled-tasks/:task_id", h.Delete)
	r.GET("/api/v1/scheduled-tasks/:task_id/runs", h.Runs)
	return r
}

func scheduledTaskRequest(engine *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func sampleTask() *schedulerv1.Task {
	next := time.Date(2026, 11, 5, 1, 30, 0, 0, time.UTC)
	return &schedulerv1.Task{
		TaskId: "task-1", Revision: 3, NextRunAt: timestamppb.New(next),
		CreateTime: timestamppb.New(next), UpdateTime: timestamppb.New(next),
		Config: &schedulerv1.TaskConfig{
			Name: "daily", Prompt: "report", Model: "model-a",
			Schedule: &schedulerv1.Schedule{Type: schedulerv1.ScheduleType_SCHEDULE_TYPE_AT,
				RunAt: "2026-11-05T09:30:00+08:00", Timezone: "Asia/Shanghai"},
		},
	}
}

func TestScheduledTaskHandlerCreateAndOwner(t *testing.T) {
	rpc := &fakeScheduledTaskRPC{task: sampleTask()}
	engine := newScheduledTaskHandlerTestEnv(rpc, true)
	w := scheduledTaskRequest(engine, http.MethodPost, "/api/v1/scheduled-tasks",
		`{"name":"daily","prompt":"report","schedule":{"type":"at","run_at":"2026-11-05T09:30:00+08:00","timezone":"Asia/Shanghai"}}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	require.Equal(t, 1, rpc.createCalls)
	require.Equal(t, "user-1", rpc.createUser)
	require.Equal(t, "daily", rpc.lastConfig.GetName())
	require.Contains(t, w.Body.String(), `"task_id":"task-1"`)
	require.Contains(t, w.Body.String(), `"next_run_at"`)
	require.Contains(t, w.Body.String(), `"revision":3`)
}

func TestScheduledTaskHandlerStatusAndValidation(t *testing.T) {
	rpc := &fakeScheduledTaskRPC{err: rpcx.Errorf(rpcx.CodeInvalidArgument, "bad schedule")}
	engine := newScheduledTaskHandlerTestEnv(rpc, true)
	w := scheduledTaskRequest(engine, http.MethodPost, "/api/v1/scheduled-tasks", `{}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "INVALID_ARGUMENT")

	rpc.err = rpcx.Errorf(rpcx.CodeNotFound, "missing")
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/scheduled-tasks/task-1", ""},
		{http.MethodPatch, "/api/v1/scheduled-tasks/task-1", `{"enabled":false}`},
		{http.MethodDelete, "/api/v1/scheduled-tasks/task-1", ""},
		{http.MethodGet, "/api/v1/scheduled-tasks/task-1/runs", ""},
	} {
		w := scheduledTaskRequest(engine, tc.method, tc.path, tc.body)
		require.Equal(t, http.StatusNotFound, w.Code, tc.method+" "+tc.path)
		require.Contains(t, w.Body.String(), "NOT_FOUND")
	}

	rpc.err = rpcx.Errorf(rpcx.CodeRevisionConflict, "stale")
	w = scheduledTaskRequest(engine, http.MethodPatch, "/api/v1/scheduled-tasks/task-1", `{"enabled":true}`)
	require.Equal(t, http.StatusConflict, w.Code)

	rpc.err = rpcx.Errorf(rpcx.CodeUnavailable, "scheduler offline")
	w = scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-tasks/task-1", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "no local fallback")

	rpc.err = nil
	w = scheduledTaskRequest(engine, http.MethodPost, "/api/v1/scheduled-tasks", `{"user_id":"user-2"}`)
	require.Equal(t, http.StatusBadRequest, w.Code, "client-supplied owner fields are rejected")

	w = scheduledTaskRequest(engine, http.MethodPost, "/api/v1/scheduled-tasks",
		`{"name":"n","prompt":"p","schedule":{"type":"cron","expression":"* * * * *","timezone":"UTC"},"session":{"mode":"existing","session_id":"s-1"}}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "INVALID_ARGUMENT")
}

func TestScheduledTaskHandlerRequiresAuthentication(t *testing.T) {
	rpc := &fakeScheduledTaskRPC{}
	engine := newScheduledTaskHandlerTestEnv(rpc, false)
	w := scheduledTaskRequest(engine, http.MethodPost, "/api/v1/scheduled-tasks", `{}`)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Zero(t, rpc.createCalls)
}

func TestScheduledTaskHandlerListRunsAndPagination(t *testing.T) {
	rpc := &fakeScheduledTaskRPC{}
	engine := newScheduledTaskHandlerTestEnv(rpc, true)
	w := scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-tasks?limit=1000&offset=2", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int32(100), rpc.listLimit)
	require.Equal(t, int32(2), rpc.listOffset)
	require.Contains(t, w.Body.String(), `"tasks":[]`)

	w = scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-tasks/task-1/runs?limit=1&offset=4", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int32(1), rpc.runsLimit)
	require.Equal(t, int32(4), rpc.runsOffset)
	require.Contains(t, w.Body.String(), `"runs":[]`)

	before := rpc.listLimit
	w = scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-tasks?limit=-1", "")
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, before, rpc.listLimit, "invalid pagination does not reach the RPC")
}

func TestScheduledTaskHandlerUpdateMergesPartialConfig(t *testing.T) {
	rpc := &fakeScheduledTaskRPC{task: sampleTask()}
	engine := newScheduledTaskHandlerTestEnv(rpc, true)
	w := scheduledTaskRequest(engine, http.MethodPatch, "/api/v1/scheduled-tasks/task-1",
		`{"name":"renamed","expected_revision":7}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotNil(t, rpc.lastUpdate)
	require.Equal(t, "renamed", rpc.lastUpdate.GetConfig().GetName())
	require.Equal(t, "report", rpc.lastUpdate.GetConfig().GetPrompt(), "unset fields keep the current value")
	require.Equal(t, int64(7), rpc.lastUpdate.GetExpectedRevision())
}

func TestScheduledTaskHandlerDeleteNoContent(t *testing.T) {
	rpc := &fakeScheduledTaskRPC{}
	engine := newScheduledTaskHandlerTestEnv(rpc, true)
	w := scheduledTaskRequest(engine, http.MethodDelete, "/api/v1/scheduled-tasks/task-1", "")
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Empty(t, w.Body.String())
	require.Equal(t, 1, rpc.deleteCalls)
}

func TestScheduledTaskHandlerInternalError(t *testing.T) {
	rpc := &fakeScheduledTaskRPC{err: errors.New("database unavailable")}
	engine := newScheduledTaskHandlerTestEnv(rpc, true)
	w := scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-tasks/task-1", "")
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Contains(t, w.Body.String(), "INTERNAL")
}

func TestOccurrenceDTOIncludesSnapshotAndActualModel(t *testing.T) {
	h := NewScheduledTaskHandler(&fakeScheduledTaskRPC{}, "model-default")
	o := &schedulerv1.Occurrence{
		OccurrenceId: "occ-1", TaskId: "task-1", SnapshotJson: `{"model":""}`,
		SessionSource: schedulerv1.SessionSource_SESSION_SOURCE_LEGACY_INTERACTIVE,
	}
	dto := h.occurrenceDTO(o)
	require.Equal(t, "legacy_interactive", dto.SessionSource)
	require.Equal(t, "model-default", dto.ActualModel)
	require.JSONEq(t, `{"model":""}`, string(dto.Snapshot))
}

var _ = context.Background
