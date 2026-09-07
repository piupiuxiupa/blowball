package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/service"
)

type handlerSubAgentStore struct {
	session  model.Session
	hasInst  bool
	run      model.SubAgentRun
	runFound bool
}

func (s *handlerSubAgentStore) GetSessionByID(context.Context, string) (*model.Session, error) {
	sess := s.session
	return &sess, nil
}

func (s *handlerSubAgentStore) GetSubAgentInstance(context.Context, string, string) (model.SubAgentInstance, bool, error) {
	return model.SubAgentInstance{AgentInstanceID: "inst-1", Name: "worker"}, s.hasInst, nil
}

func (s *handlerSubAgentStore) GetSubAgentRun(context.Context, string, string, string) (model.SubAgentRun, bool, error) {
	return s.run, s.runFound, nil
}

func (s *handlerSubAgentStore) ListSubAgentRuns(context.Context, string, string) ([]model.SubAgentRun, error) {
	return []model.SubAgentRun{s.run}, nil
}

func newSubAgentHandlerTestRouter(t *testing.T, store *handlerSubAgentStore) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewSubAgentHandler(service.NewSubAgentService(store))
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-1")
		c.Next()
	})
	r.GET("/api/v1/sessions/:session_id/subagents/:agent_instance_id/runs", h.ListRuns)
	r.GET("/api/v1/sessions/:session_id/subagents/:agent_instance_id/runs/:run_id", h.GetRun)
	return r
}

func handlerSubAgentRun() model.SubAgentRun {
	now := time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC)
	return model.SubAgentRun{
		SessionID: "sess-1", AgentInstanceID: "inst-1", RunID: "run-1", RunNo: 1,
		Name: "worker", Status: model.SubAgentStatusCompleted, MessageCount: 2,
		StartedAt: now, FinishedAt: now.Add(time.Minute), MessagesJSON: []byte(`[
			{"role":"system","content":"secret"},
			{"role":"user","content":"task"},
			{"role":"assistant","content":"answer","tool_calls":[
				{"id":"tool-call","function":{"name":"executor","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"tool-call","name":"executor","content":"ok"}
		]`),
	}
}

func TestSubAgentHandlers_ListAndDetail(t *testing.T) {
	run := handlerSubAgentRun()
	r := newSubAgentHandlerTestRouter(t, &handlerSubAgentStore{
		session: model.Session{SessionID: "sess-1", UserID: "user-1"},
		hasInst: true, run: run, runFound: true,
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/sessions/sess-1/subagents/inst-1/runs", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "messages_json")
	assert.NotContains(t, w.Body.String(), "secret")

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet,
		"/api/v1/sessions/sess-1/subagents/inst-1/runs/run-1", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	for _, forbidden := range []string{
		"messages_json", "tools_json", "system_prompt", "resume_eligible",
		"base_message_count", "context_message_count", "context_bytes",
	} {
		assert.NotContains(t, body, forbidden)
	}
	assert.NotContains(t, w.Body.String(), "secret")
	assert.Contains(t, w.Body.String(), `"type":"task"`)
	assert.Contains(t, w.Body.String(), `"type":"assistant"`)
	assert.Contains(t, w.Body.String(), `"tool_call_id"`)
}

func TestSubAgentHandlers_NotFound(t *testing.T) {
	tests := []struct {
		name  string
		store *handlerSubAgentStore
		path  string
	}{
		{
			name: "wrong owner",
			store: &handlerSubAgentStore{
				session: model.Session{SessionID: "sess-1", UserID: "other"}, hasInst: true,
			},
			path: "/api/v1/sessions/sess-1/subagents/inst-1/runs",
		},
		{
			name: "missing instance",
			store: &handlerSubAgentStore{
				session: model.Session{SessionID: "sess-1", UserID: "user-1"}, hasInst: false,
			},
			path: "/api/v1/sessions/sess-1/subagents/inst-1/runs",
		},
		{
			name: "active or missing run",
			store: &handlerSubAgentStore{
				session: model.Session{SessionID: "sess-1", UserID: "user-1"}, hasInst: true,
			},
			path: "/api/v1/sessions/sess-1/subagents/inst-1/runs/run-1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newSubAgentHandlerTestRouter(t, tc.store)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r.ServeHTTP(w, req)
			assert.Equal(t, http.StatusNotFound, w.Code)
			assert.Contains(t, w.Body.String(), `"code":"NOT_FOUND"`)
		})
	}
}
