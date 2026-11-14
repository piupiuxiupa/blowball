package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/pkg/middleware"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/resource"
	"github.com/lush/blowball/pkg/service"
)

type fakeScheduledSessions struct {
	summaries []service.ScheduledSessionSummary
	detail    *service.ScheduledSessionDetail
	messages  []model.Message
	calls     []model.ToolCall
	err       error
	deleted   []string
}

func (f *fakeScheduledSessions) List(context.Context, string, string, int, int) ([]service.ScheduledSessionSummary, error) {
	return f.summaries, f.err
}
func (f *fakeScheduledSessions) GetDetail(context.Context, string, string) (*service.ScheduledSessionDetail, error) {
	return f.detail, f.err
}
func (f *fakeScheduledSessions) Messages(context.Context, string, string, string, int, string, string) ([]model.Message, string, error) {
	return f.messages, "next", f.err
}
func (f *fakeScheduledSessions) ToolCalls(context.Context, string, string) ([]model.ToolCall, error) {
	return f.calls, nil
}
func (f *fakeScheduledSessions) Delete(_ context.Context, _, sessionID string) error {
	f.deleted = append(f.deleted, sessionID)
	return f.err
}

type fakeOccurrenceLookup struct {
	resp *schedulerv1.GetOccurrenceResponse
}

func (f fakeOccurrenceLookup) GetOccurrence(context.Context, *schedulerv1.GetOccurrenceRequest) (*schedulerv1.GetOccurrenceResponse, error) {
	return f.resp, nil
}

func scheduledSessionEnv(authenticated bool, svc *fakeScheduledSessions, occ ScheduledOccurrenceLookup) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewScheduledSessionHandler(svc, occ, "model-default")
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if authenticated {
			c.Set(middleware.UserIDKey, "user-1")
		}
		c.Next()
	})
	r.GET("/api/v1/scheduled-sessions", h.List)
	r.GET("/api/v1/scheduled-sessions/:session_id", h.Get)
	r.GET("/api/v1/scheduled-sessions/:session_id/messages", h.Messages)
	r.DELETE("/api/v1/scheduled-sessions/:session_id", h.Delete)
	return r
}

func TestScheduledSessionHandlerListAndDetail(t *testing.T) {
	svc := &fakeScheduledSessions{
		summaries: []service.ScheduledSessionSummary{{
			SessionID: "ss-1", TaskID: "task-1", OccurrenceID: "occ-1", RunID: "run-1",
			Source: model.SessionSourceScheduled, Title: "nightly",
		}},
		detail: &service.ScheduledSessionDetail{Session: model.Session{
			SessionID: "ss-1", UserID: "user-1", TaskID: "task-1", OccurrenceID: "occ-1", RunID: "run-1",
			CreateTime: time.Unix(1_700_000_000, 0).UTC(), UpdateTime: time.Unix(1_700_000_001, 0).UTC(),
		}, Title: "nightly"},
	}
	occ := fakeOccurrenceLookup{resp: &schedulerv1.GetOccurrenceResponse{Occurrence: &schedulerv1.Occurrence{
		OccurrenceId: "occ-1", SnapshotJson: `{"model":""}`, SessionSource: schedulerv1.SessionSource_SESSION_SOURCE_SCHEDULED,
	}}}
	engine := scheduledSessionEnv(true, svc, occ)

	w := scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-sessions?task_id=task-1", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"session_id":"ss-1"`)
	require.Contains(t, w.Body.String(), `"session_source":"scheduled"`)

	w = scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-sessions/ss-1", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"snapshot":{"model":""}`)
	require.Contains(t, w.Body.String(), `"actual_model":"model-default"`)
}

func TestScheduledSessionHandlerRejectsOrdinaryAndForeign(t *testing.T) {
	svc := &fakeScheduledSessions{detail: nil}
	engine := scheduledSessionEnv(true, svc, nil)
	w := scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-sessions/ordinary-id", "")
	require.Equal(t, http.StatusNotFound, w.Code)

	svc.err = service.ErrScheduledSessionNotFound
	w = scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-sessions/foreign", "")
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestScheduledSessionHandlerDeleteStates(t *testing.T) {
	svc := &fakeScheduledSessions{}
	engine := scheduledSessionEnv(true, svc, nil)
	w := scheduledTaskRequest(engine, http.MethodDelete, "/api/v1/scheduled-sessions/ss-1", "")
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, []string{"ss-1"}, svc.deleted)

	svc.err = service.ErrScheduledSessionActive
	w = scheduledTaskRequest(engine, http.MethodDelete, "/api/v1/scheduled-sessions/ss-1", "")
	require.Equal(t, http.StatusConflict, w.Code)
	require.Contains(t, w.Body.String(), "EXECUTION_STOP_NOT_PROVEN")

	svc.err = service.ErrScheduledSessionNotFound
	w = scheduledTaskRequest(engine, http.MethodDelete, "/api/v1/scheduled-sessions/ss-1", "")
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestScheduledSessionHandlerRequiresAuth(t *testing.T) {
	engine := scheduledSessionEnv(false, &fakeScheduledSessions{}, nil)
	w := scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-sessions", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

type fakeResources struct {
	draft     *resource.DraftInfo
	versions  []model.ScheduledResourceVersion
	published *model.ScheduledResourceVersion
	err       error
}

func (f fakeResources) GetDraft(context.Context, string, string) (*resource.DraftInfo, error) {
	return f.draft, f.err
}
func (f fakeResources) ListVersions(context.Context, string, string) ([]model.ScheduledResourceVersion, error) {
	return f.versions, f.err
}
func (f fakeResources) Publish(context.Context, string, string, uint64, string) (*model.ScheduledResourceVersion, error) {
	return f.published, f.err
}

func resourceEnv(authenticated bool, svc ScheduledResourceService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewScheduledResourceHandler(svc)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if authenticated {
			c.Set(middleware.UserIDKey, "user-1")
		}
		c.Next()
	})
	r.GET("/api/v1/scheduled-tasks/:task_id/resources", h.Get)
	r.POST("/api/v1/scheduled-tasks/:task_id/resources/publish", h.Publish)
	return r
}

func TestScheduledResourceHandlerGetAndPublish(t *testing.T) {
	svc := fakeResources{
		draft:     &resource.DraftInfo{TaskID: "task-1", DraftRevision: 4, Files: []resource.FileEntry{{Path: "a.txt", Size: 3}}},
		versions:  []model.ScheduledResourceVersion{{TaskID: "task-1", Revision: 2, ManifestHash: "h", FileCount: 1, TotalBytes: 3}},
		published: &model.ScheduledResourceVersion{TaskID: "task-1", Revision: 3, ManifestHash: "h3", FileCount: 2, TotalBytes: 9},
	}
	engine := resourceEnv(true, svc)
	w := scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-tasks/task-1/resources", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"draft_revision":4`)
	require.Contains(t, w.Body.String(), `"resource_revision":2`)

	w = scheduledTaskRequest(engine, http.MethodPost, "/api/v1/scheduled-tasks/task-1/resources/publish",
		`{"expected_draft_revision":4,"request_id":"req-1"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"resource_revision":3`)
}

func TestScheduledResourceHandlerErrors(t *testing.T) {
	engine := resourceEnv(true, fakeResources{err: resource.ErrDraftConflict})
	w := scheduledTaskRequest(engine, http.MethodPost, "/api/v1/scheduled-tasks/task-1/resources/publish",
		`{"request_id":"req-1"}`)
	require.Equal(t, http.StatusConflict, w.Code)
	require.Contains(t, w.Body.String(), "REVISION_CONFLICT")

	engine = resourceEnv(true, fakeResources{err: resource.ErrNotFound})
	w = scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-tasks/foreign/resources", "")
	require.Equal(t, http.StatusNotFound, w.Code)

	engine = resourceEnv(true, fakeResources{err: resource.ErrUnavailable})
	w = scheduledTaskRequest(engine, http.MethodGet, "/api/v1/scheduled-tasks/task-1/resources", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}
