package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/handler"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/msgflush"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/store/fs"
	redisstore "github.com/lush/blowball/internal/store/redis"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/skill"
)

// roleTestEnv builds two engines against ONE shared set of fakes (fs, miniredis,
// in-memory MySQL): an api-only engine (RegisterHealthz + RegisterAPIRoutes)
// and an agent-only engine (RegisterHealthz + RegisterAgentRoutes). This mirrors
// how serveRun wires the two process roles against the same data plane, so the
// route-ownership assertions exercise the real handler constructors rather than
// stubs.
type roleTestEnv struct {
	apiEngine   *gin.Engine
	agentEngine *gin.Engine
	mysqlFake   *memoryMySQL
	redisSvc    *redisstore.Store
}

func newRoleTestEnv(t *testing.T, llm agent.LLMClient) *roleTestEnv {
	t.Helper()
	t.Cleanup(func() { goleak.VerifyNone(t) })

	dataDir := t.TempDir()
	fsSvc, err := fs.New(dataDir)
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	redisSvc, err := redisstore.New(mr.Addr(), "", 0, time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisSvc.Close() })

	mysqlFake := newMemoryMySQL()
	require.NoError(t, mysqlFake.CreateSession(context.Background(), model.Session{
		SessionID: defaultSessionID,
		UserID:    defaultUserID,
		TraceID:   "seed-trace",
	}))

	deps := service.SessionDeps{
		MySQL: mysqlFake,
		Redis: redisSvc,
		FS:    fsSvc,
		DrainMessageQueue: func(ctx context.Context) error {
			return msgflush.Drain(ctx, redisSvc, mysqlFake)
		},
	}
	sessSvc := service.NewSessionService(deps)
	msgSvc := service.NewMessageService(deps, sessSvc.SaveMessage)
	titleSvc := service.NewTitleService(llm, mysqlFake, config.OpenAIConfig{TitleModel: "title-model"})

	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{APIKey: "test", Models: testCatalog()},
		JWT:    config.JWTConfig{Secret: integrationTestSecret, Expire: "1h"},
		Agents: agentConfig(),
	}
	orch, err := agent.NewOrchestrator(llm, cfg, tool.NewRegistry(), nil, skill.NewLoader("", nil), nil, mysqlFake)
	require.NoError(t, err)

	// Real handler constructors — the same ones serveRun's wireAPI / wireAgent
	// use. Login is stubbed (the harness does the same) since these tests assert
	// route ownership, not the login flow.
	sessH := handler.NewSessionHandler(sessSvc, titleSvc, redisSvc.RunStore())
	runMgr := run.NewManager(redisSvc.RunStore(), run.NewRegistry())
	turnRunH := handler.NewTurnRunHandler(runMgr)
	streamH := handler.NewMessageStreamHandler(sessSvc, msgSvc, titleSvc, nil, nil, handler.NewOrchestratorAdapter(orch), dataDir, runMgr, handler.NewModelSelectionConfig(cfg), 0)
	wsH := handler.NewWorkspaceHandler(fsSvc, 1<<20, handler.OnlyOfficeSettings{})
	mcpH := handler.NewMCPHandler(tool.NewRegistry(), nil, fsSvc.UserWorkspace)
	skillH := handler.NewSkillHandler(fsSvc, nil)

	authMW := middleware.AuthMiddleware(integrationTestSecret)
	apiDeps := handler.RouteDeps{
		AuthMW:                      authMW,
		QueryTokenAuthMW:            middleware.QueryTokenAuthMiddleware(integrationTestSecret),
		Login:                       func(*gin.Context) {},
		SessionList:                 sessH.ListSessions,
		SessionCreate:               sessH.CreateSession,
		SessionGet:                  sessH.GetSession,
		SessionMessages:             sessH.GetSessionMessages,
		SubAgentRuns:                handler.NewSubAgentHandler(service.NewSubAgentService(mysqlFake)).ListRuns,
		SubAgentRunDetail:           handler.NewSubAgentHandler(service.NewSubAgentService(mysqlFake)).GetRun,
		SessionDelete:               sessH.DeleteSession,
		SessionUpdateTitle:          sessH.UpdateTitle,
		WorkspaceList:               wsH.List,
		WorkspaceUpload:             wsH.Upload,
		WorkspaceSearch:             wsH.Search,
		WorkspaceDownload:           wsH.Download,
		WorkspaceTokenDownload:      wsH.TokenDownload,
		WorkspaceContent:            wsH.Content,
		WorkspaceDelete:             wsH.Delete,
		WorkspaceRename:             wsH.Rename,
		WorkspaceOnlyOfficeConfig:   wsH.OnlyOfficeConfig,
		WorkspaceOnlyOfficeCallback: wsH.OnlyOfficeCallback,
		SkillsList:                  skillH.List,
	}
	agentDeps := handler.RouteDeps{
		AuthMW:      authMW,
		SendMessage: streamH.SendMessage,
		TurnCancel:  turnRunH.CancelTurn,
		TurnEvents:  turnRunH.TurnEvents,
		MCPTools:    mcpH.Tools,
	}

	apiEngine := gin.New()
	apiEngine.Use(middleware.TraceMiddleware())
	handler.RegisterHealthz(apiEngine)
	handler.RegisterAPIRoutes(apiEngine, apiDeps)

	agentEngine := gin.New()
	agentEngine.Use(middleware.TraceMiddleware())
	handler.RegisterHealthz(agentEngine)
	handler.RegisterAgentRoutes(agentEngine, agentDeps)

	return &roleTestEnv{apiEngine: apiEngine, agentEngine: agentEngine, mysqlFake: mysqlFake, redisSvc: redisSvc}
}

// TestAPIRoleEngine_OwnsCRUDRejectsAgentRoutes asserts the api-role engine
// serves its CRUD routes (GET /sessions) and refuses the agent-owned routes
// (POST /sessions/:id/messages, GET /mcp/tools) with 404.
func TestAPIRoleEngine_OwnsCRUDRejectsAgentRoutes(t *testing.T) {
	llm := newScriptedLLMClient()
	env := newRoleTestEnv(t, llm)
	token := authToken(t, defaultUserID)

	// CRUD route is served by the api engine.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	env.apiEngine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "api engine should serve GET /sessions; body: %s", w.Body.String())

	// The single-session detail read is served by the api engine too.
	detailReq := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+defaultSessionID, nil)
	detailReq.Header.Set("Authorization", "Bearer "+token)
	detailW := httptest.NewRecorder()
	env.apiEngine.ServeHTTP(detailW, detailReq)
	assert.Equal(t, http.StatusOK, detailW.Code, "api engine should serve GET /sessions/:id; body: %s", detailW.Body.String())

	// The workspace search endpoint is served by the api engine (an empty or
	// missing workspace root legitimately returns 200 with zero entries).
	searchReq := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/search?pattern=x", nil)
	searchReq.Header.Set("Authorization", "Bearer "+token)
	searchW := httptest.NewRecorder()
	env.apiEngine.ServeHTTP(searchW, searchReq)
	assert.Equal(t, http.StatusOK, searchW.Code, "api engine should serve GET /workspace/search; body: %s", searchW.Body.String())

	// Agent-owned routes are absent from the api engine (404, not 401).
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/sessions/" + defaultSessionID + "/messages"},
		{http.MethodGet, "/api/v1/mcp/tools"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		env.apiEngine.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code, "api engine: %s %s should be 404", tc.method, tc.path)
	}
}

// TestAgentRoleEngine_OwnsStreamingRejectsCRUD asserts the agent-role engine
// serves its routes (the streaming endpoint and GET /mcp/tools) and refuses the
// CRUD routes with 404. It also confirms a streaming turn run under the agent
// engine persists through the shared store (the agent owns the full pipeline).
func TestAgentRoleEngine_OwnsStreamingRejectsCRUD(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"Hi"},
			content:      "Hi",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
		// TitleService fires on the first turn; keep the queue non-empty.
		scriptedLLMResponse{content: "T", finishReason: "stop"},
	)
	env := newRoleTestEnv(t, llm)
	token := authToken(t, defaultUserID)

	// CRUD routes are absent from the agent engine (404, not 401/500).
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/sessions"},
		{http.MethodPost, "/api/v1/sessions"},
		{http.MethodGet, "/api/v1/sessions/" + defaultSessionID},
		{http.MethodGet, "/api/v1/workspace/search"},
		{http.MethodGet, "/api/v1/skills"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		env.agentEngine.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code, "agent engine: %s %s should be 404", tc.method, tc.path)
	}

	// The streaming endpoint is served by the agent engine and runs the full
	// pipeline (lookup → recover → orchestrate → SSE → persist) in-process.
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/"+defaultSessionID+"/messages",
		strings.NewReader(`{"content":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	env.agentEngine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "agent engine should serve the streaming endpoint; body: %s", w.Body.String())
	require.Equal(t, "text/event-stream", w.Result().Header.Get("Content-Type"))

	// The agent-owned pipeline persisted the turn into the shared store (dual
	// write, then drain the write-behind queue into the fake MySQL tier).
	waitForQueuedTurn(t, env.redisSvc, env.mysqlFake, defaultSessionID, 1)

	// GET /mcp/tools is served by the agent engine.
	mcpReq := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/tools", nil)
	mcpReq.Header.Set("Authorization", "Bearer "+token)
	mcpW := httptest.NewRecorder()
	env.agentEngine.ServeHTTP(mcpW, mcpReq)
	assert.Equal(t, http.StatusOK, mcpW.Code, "agent engine should serve GET /mcp/tools")
}
