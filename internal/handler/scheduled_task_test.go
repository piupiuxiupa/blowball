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

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/service"
)

type fakeScheduledTaskService struct {
	task        *model.ScheduledTask
	err         error
	createCalls int
	createUser  string
	listLimit   int
	listOffset  int
	runsLimit   int
	runsOffset  int
	deleteCalls int
}

func (f *fakeScheduledTaskService) Create(_ context.Context, userID string, _ service.CreateScheduledTaskInput) (*model.ScheduledTask, error) {
	f.createCalls++
	f.createUser = userID
	return f.task, f.err
}
func (f *fakeScheduledTaskService) Get(context.Context, string, string) (*model.ScheduledTask, error) {
	return f.task, f.err
}
func (f *fakeScheduledTaskService) List(_ context.Context, _ string, limit, offset int) ([]model.ScheduledTask, error) {
	f.listLimit, f.listOffset = limit, offset
	if f.err != nil {
		return nil, f.err
	}
	if f.task != nil {
		return []model.ScheduledTask{*f.task}, nil
	}
	return nil, nil
}
func (f *fakeScheduledTaskService) Update(context.Context, string, string, service.UpdateScheduledTaskInput) (*model.ScheduledTask, error) {
	return f.task, f.err
}
func (f *fakeScheduledTaskService) Delete(context.Context, string, string) error {
	f.deleteCalls++
	return f.err
}
func (f *fakeScheduledTaskService) Runs(_ context.Context, _, _ string, limit, offset int) ([]model.ScheduledTaskRun, error) {
	f.runsLimit, f.runsOffset = limit, offset
	return nil, f.err
}

func newScheduledTaskHandlerTestEnv(svc *fakeScheduledTaskService, authenticated bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewScheduledTaskHandler(svc)
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

func TestScheduledTaskHandlerCreateAndOwner(t *testing.T) {
	next := time.Date(2026, 11, 5, 1, 30, 0, 0, time.UTC)
	svc := &fakeScheduledTaskService{task: &model.ScheduledTask{TaskID: "task-1", Name: "daily", NextRunAt: &next}}
	engine := newScheduledTaskHandlerTestEnv(svc, true)
	w := scheduledTaskRequest(engine, http.MethodPost, "/api/v1/scheduled-tasks", `{"name":"daily","prompt":"report","schedule":{"type":"at","run_at":"2026-11-05T09:30:00+08:00","timezone":"Asia/Shanghai"}}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	require.Equal(t, 1, svc.createCalls)
	require.Equal(t, "user-1", svc.createUser)
	require.Contains(t, w.Body.String(), `"task_id":"task-1"`)
	require.Contains(t, w.Body.String(), `"next_run_at"`)
}

func TestScheduledTaskHandlerStatusAndValidation(t *testing.T) {
	svc := &fakeScheduledTaskService{err: &service.ScheduledTaskError{Code: "INVALID_SCHEDULE", Message: "bad schedule"}}
	engine := newScheduledTaskHandlerTestEnv(svc, true)
	w := scheduledTaskRequest(engine, http.MethodPost, "/api/v1/scheduled-tasks", `{}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "INVALID_SCHEDULE")

	svc.err = service.ErrScheduledTaskNotFound
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

	svc.err = nil
	w = scheduledTaskRequest(engine, http.MethodPost, "/api/v1/scheduled-tasks", `{"user_id":"user-2"}`)
	require.Equal(t, http.StatusBadRequest, w.Code, "client-supplied owner fields are rejected")
}

func TestScheduledTaskHandlerRequiresAuthentication(t *testing.T) {
	svc := &fakeScheduledTaskService{}
	engine := newScheduledTaskHandlerTestEnv(svc, false)
	w := scheduledTaskRequest(engine, http.MethodPost, "/api/v1/scheduled-tasks", `{}`)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Zero(t, svc.createCalls)
}

func TestScheduledTaskHandlerListRunsAndPagination(t *testing.T) {
	svc := &fakeScheduledTaskService{}
	engine := newScheduledTaskHandlerTestEnv(svc, true)
	w := scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-tasks?limit=1000&offset=2", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 100, svc.listLimit)
	require.Equal(t, 2, svc.listOffset)
	require.Contains(t, w.Body.String(), `"tasks":[]`)

	w = scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-tasks/task-1/runs?limit=1&offset=4", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, svc.runsLimit)
	require.Equal(t, 4, svc.runsOffset)
	require.Contains(t, w.Body.String(), `"runs":[]`)

	before := svc.listLimit
	w = scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-tasks?limit=-1", "")
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, before, svc.listLimit, "invalid pagination does not reach the service")
}

func TestScheduledTaskHandlerDeleteNoContent(t *testing.T) {
	svc := &fakeScheduledTaskService{}
	engine := newScheduledTaskHandlerTestEnv(svc, true)
	w := scheduledTaskRequest(engine, http.MethodDelete, "/api/v1/scheduled-tasks/task-1", "")
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Empty(t, w.Body.String())
	require.Equal(t, 1, svc.deleteCalls)
}

func TestScheduledTaskHandlerInternalError(t *testing.T) {
	svc := &fakeScheduledTaskService{err: errors.New("database unavailable")}
	engine := newScheduledTaskHandlerTestEnv(svc, true)
	w := scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-tasks/task-1", "")
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Contains(t, w.Body.String(), "INTERNAL")
}
