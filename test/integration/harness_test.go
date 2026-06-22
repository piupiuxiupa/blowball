// Package integration exercises the blowball backend end-to-end through the
// HTTP layer with no external dependencies: MySQL is replaced by an in-memory
// fake, Redis by miniredis, and the LLM by a scripted fake. The real FS store,
// real services, real handlers, real agent orchestrator, and real SSE writer
// are all on the critical path so these tests catch wiring regressions the
// unit tests in each package cannot.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/handler"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	cursorpkg "github.com/lush/blowball/internal/pkg/cursor"
	"github.com/lush/blowball/internal/pkg/jwt"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/store/fs"
	mysqlstore "github.com/lush/blowball/internal/store/mysql"
	redisstore "github.com/lush/blowball/internal/store/redis"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/skill"
)

const (
	integrationTestSecret = "integration-test-secret"
	defaultUserID         = "user-int"
	defaultSessionID      = "sess-int"
)

func init() {
	gin.SetMode(gin.TestMode)
	// Tests run with the no-op default logger; assign a Nop explicitly so any
	// earlier test that called logger.Init cannot leak a noisy JSON logger in.
	logger.SetDefault(logger.L())
}

// scriptedLLMResponse is one entry in a scriptedLLMClient's queue. The client
// pops these FIFO across StreamChat calls, emitting tokens via onToken before
// returning the aggregated response. err short-circuits StreamChat if set.
type scriptedLLMResponse struct {
	tokens       []string
	content      string
	finishReason string
	toolCalls    []agent.ToolCall
	usage        agent.Usage
	err          error
}

// scriptedLLMClient is a fake agent.LLMClient shared across the agent tree.
// Each StreamChat call pops the next prepared response; a panic surfaces if
// the queue runs dry, since that always indicates a test-authoring bug. The
// client is concurrency-safe because parallel sub-agent dispatch hits the same
// client from multiple goroutines.
type scriptedLLMClient struct {
	mu        sync.Mutex
	responses []scriptedLLMResponse
	calls     []agent.LLMRequest
}

func newScriptedLLMClient(responses ...scriptedLLMResponse) *scriptedLLMClient {
	return &scriptedLLMClient{responses: responses}
}

func (c *scriptedLLMClient) StreamChat(ctx context.Context, req agent.LLMRequest, onToken func(string) error) (agent.LLMResponse, error) {
	c.mu.Lock()
	if len(c.responses) == 0 {
		c.mu.Unlock()
		panic("scriptedLLMClient: responses queue exhausted; test scripted too few LLM rounds")
	}
	resp := c.responses[0]
	c.responses = c.responses[1:]
	c.calls = append(c.calls, req)
	c.mu.Unlock()

	if resp.err != nil {
		return agent.LLMResponse{}, resp.err
	}

	for _, tok := range resp.tokens {
		if err := ctx.Err(); err != nil {
			return agent.LLMResponse{FinishReason: resp.finishReason, Content: resp.content, ToolCalls: resp.toolCalls, Usage: resp.usage}, err
		}
		if onToken != nil {
			if err := onToken(tok); err != nil {
				return agent.LLMResponse{FinishReason: resp.finishReason, Content: resp.content, ToolCalls: resp.toolCalls, Usage: resp.usage}, err
			}
		}
	}
	return agent.LLMResponse{
		FinishReason: resp.finishReason,
		Content:      resp.content,
		ToolCalls:    resp.toolCalls,
		Usage:        resp.usage,
	}, nil
}

// callCount returns the total number of StreamChat invocations observed.
func (c *scriptedLLMClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// requests returns a snapshot of every LLMRequest observed by StreamChat.
func (c *scriptedLLMClient) requests() []agent.LLMRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]agent.LLMRequest, len(c.calls))
	copy(out, c.calls)
	return out
}

// memoryMySQL is an in-memory service.MySQLStore. It records messages by
// session and supports the same GetSessionByID / ListMessages calls the real
// sqlx-backed store does, so integration tests can verify the third
// persistence tier without spinning up MySQL.
type memoryMySQL struct {
	mu       sync.Mutex
	sessions map[string]*model.Session
	titles   map[string]model.Title
	messages map[string][]model.Message
	nextID   int64
}

func newMemoryMySQL() *memoryMySQL {
	return &memoryMySQL{
		sessions: map[string]*model.Session{},
		titles:   map[string]model.Title{},
		messages: map[string][]model.Message{},
	}
}

func (m *memoryMySQL) CreateSession(_ context.Context, sess model.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[sess.SessionID]; ok {
		return fmt.Errorf("memoryMySQL: duplicate session %q", sess.SessionID)
	}
	cp := sess
	m.sessions[sess.SessionID] = &cp
	return nil
}

func (m *memoryMySQL) GetSessionByID(_ context.Context, sessionID string) (*model.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[sessionID]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, nil
}

func (m *memoryMySQL) ListSessionsWithTitle(_ context.Context, userID string) ([]mysqlstore.SessionWithTitle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mysqlstore.SessionWithTitle, 0)
	for _, s := range m.sessions {
		if s.UserID != userID {
			continue
		}
		row := mysqlstore.SessionWithTitle{
			SessionID:  s.SessionID,
			UserID:     s.UserID,
			TraceID:    s.TraceID,
			UpdateTime: time.Now().UTC(),
		}
		if t, ok := m.titles[s.SessionID]; ok {
			row.Title = t.Title
		}
		out = append(out, row)
	}
	return out, nil
}

func (m *memoryMySQL) UpsertTitle(_ context.Context, t model.Title) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.titles[t.SessionID] = t
	return nil
}

func (m *memoryMySQL) GetTitle(_ context.Context, sessionID string) (*model.Title, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.titles[sessionID]; ok {
		cp := t
		return &cp, nil
	}
	return nil, nil
}

func (m *memoryMySQL) AppendMessage(_ context.Context, msg model.Message) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	msg.ID = m.nextID
	msg.UpdateTime = time.Now().UTC()
	m.messages[msg.SessionID] = append(m.messages[msg.SessionID], msg)
	return msg.ID, nil
}

func (m *memoryMySQL) AppendMessages(_ context.Context, msgs []model.Message) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]int64, 0, len(msgs))
	for i := range msgs {
		m.nextID++
		msgs[i].ID = m.nextID
		msgs[i].UpdateTime = time.Now().UTC()
		m.messages[msgs[i].SessionID] = append(m.messages[msgs[i].SessionID], msgs[i])
		ids = append(ids, msgs[i].ID)
	}
	return ids, nil
}

func (m *memoryMySQL) ListMessages(_ context.Context, sessionID string) ([]model.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Message, len(m.messages[sessionID]))
	copy(out, m.messages[sessionID])
	return out, nil
}

func (m *memoryMySQL) ListMessagesPaged(_ context.Context, sessionID, cursorStr string, pageSize int, order string) ([]model.Message, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := make([]model.Message, len(m.messages[sessionID]))
	copy(rows, m.messages[sessionID])

	less := func(i, j int) bool {
		if rows[i].MsgTime.Equal(rows[j].MsgTime) {
			if rows[i].MsgIndex == rows[j].MsgIndex {
				return rows[i].ID < rows[j].ID
			}
			return rows[i].MsgIndex < rows[j].MsgIndex
		}
		return rows[i].MsgTime.Before(rows[j].MsgTime)
	}
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if !less(i, j) {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	if order == "desc" {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}

	if pageSize < 1 {
		pageSize = 1
	}
	if pageSize > 200 {
		pageSize = 200
	}

	start := 0
	if cursorStr != "" {
		for i, msg := range rows {
			enc, err := cursorpkg.Encode(cursorpkg.Cursor{MsgTime: msg.MsgTime, MsgIndex: msg.MsgIndex, ID: msg.ID})
			if err != nil {
				return nil, "", err
			}
			if enc == cursorStr {
				start = i + 1
				break
			}
		}
	}
	end := start + pageSize
	if end > len(rows) {
		end = len(rows)
	}
	page := rows[start:end]
	if len(page) == 0 {
		return page, "", nil
	}
	if end >= len(rows) {
		return page, "", nil
	}
	last := page[len(page)-1]
	next, err := cursorpkg.Encode(cursorpkg.Cursor{MsgTime: last.MsgTime, MsgIndex: last.MsgIndex, ID: last.ID})
	if err != nil {
		return nil, "", err
	}
	return page, next, nil
}

// messagesFor returns the recorded messages for sessionID. Test helper.
func (m *memoryMySQL) messagesFor(sessionID string) []model.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Message, len(m.messages[sessionID]))
	copy(out, m.messages[sessionID])
	return out
}

// testEnv is the wired-up integration harness: real handlers + real services
// + real agent orchestrator backed by a scripted LLM, real FS, real Redis
// (miniredis), and an in-memory MySQL fake.
type testEnv struct {
	t         *testing.T
	engine    *gin.Engine
	dataDir   string
	fsSvc     *fs.Store
	redisSvc  *redisstore.Store
	miniRedis *miniredis.Miniredis
	mysqlFake *memoryMySQL
	llm       agent.LLMClient
	sessSvc   *service.SessionService
	msgSvc    *service.MessageService
}

// agentConfig builds the per-agent config used by the orchestrator. Chongzhi
// is granted the Xizhi file tools so file_ops_test can drive a real
// xizhi_write_file invocation; Confuse carries no plain tools (it dispatches
// via the synthetic invoke_* entries the orchestrator injects automatically).
func agentConfig() config.AgentsConfig {
	return config.AgentsConfig{
		Confuse: config.AgentConfig{
			Name:         stream.AgentConfuse,
			Model:        "gpt-test",
			SystemPrompt: "you are confuse",
			MaxTokens:    512,
			Tools:        []string{},
		},
		Chongzhi: config.AgentConfig{
			Name:         stream.AgentChongzhi,
			Model:        "gpt-test",
			SystemPrompt: "you are chongzhi",
			MaxTokens:    512,
			Tools:        []string{"xizhi_write_file", "xizhi_read_file", "xizhi_modify_file"},
		},
		Liang: config.AgentConfig{
			Name:         stream.AgentLiang,
			Model:        "gpt-test",
			SystemPrompt: "you are liang",
			MaxTokens:    512,
			Tools:        []string{},
		},
	}
}

// newTestEnv wires the full handler stack against in-process fakes. The
// returned engine has every /api/v1 route mounted with real auth middleware
// gated by integrationTestSecret; use authToken() to mint a Bearer token the
// middleware will accept. llm may be a *scriptedLLMClient or any wrapper that
// satisfies agent.LLMClient (e.g. the parallel test's trackingLLMClient).
//
// The returned engine wires goleak verification through t.Cleanup, registered
// BEFORE the miniredis cleanup so the LIFO cleanup order drains miniredis
// (and the redis client) first and only then samples for leaked goroutines.
func newTestEnv(t *testing.T, llm agent.LLMClient) *testEnv {
	t.Helper()

	// Register goleak FIRST so it runs LAST in the LIFO cleanup chain — after
	// the miniredis and redis.client goroutines have exited.
	t.Cleanup(func() { goleak.VerifyNone(t) })

	dataDir := t.TempDir()

	fsSvc, err := fs.New(dataDir)
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	// Use the production constructor — it pings miniredis exactly like a real
	// server and the connection succeeds, exercising the same bootstrap path
	// main.go uses.
	redisSvc, err := redisstore.New(mr.Addr(), "", 0, time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisSvc.Close() })

	mysqlFake := newMemoryMySQL()
	// Seed a session for the default user so legacy message-flow tests can
	// continue posting to the fixed defaultSessionID without first calling
	// POST /sessions.
	require.NoError(t, mysqlFake.CreateSession(context.Background(), model.Session{
		SessionID: defaultSessionID,
		UserID:    defaultUserID,
		TraceID:   "seed-trace",
	}))

	deps := service.SessionDeps{MySQL: mysqlFake, Redis: redisSvc, FS: fsSvc}
	sessSvc := service.NewSessionService(deps)
	msgSvc := service.NewMessageService(deps, sessSvc.SaveMessage)
	// TitleService is given the same scripted LLM; for these tests we never
	// script a title round so it falls back to the first-20-chars heuristic.
	titleSvc := service.NewTitleService(llm, mysqlFake, config.OpenAIConfig{Model: "title-model"})

	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{APIKey: "test", Model: "gpt-test"},
		JWT:    config.JWTConfig{Secret: integrationTestSecret, Expire: "1h"},
		Agents: agentConfig(),
	}
	baseReg := tool.NewRegistry()
	orch, err := agent.NewOrchestrator(llm, cfg, baseReg, nil, skill.NewLoader("", nil), nil)
	require.NoError(t, err)

	sessH := handler.NewSessionHandler(sessSvc, msgSvc, titleSvc, handler.NewOrchestratorAdapter(orch), dataDir)
	wsH := handler.NewWorkspaceHandler(fsSvc, 1<<20)
	mcpH := handler.NewMCPHandler(tool.NewRegistry())
	skillH := handler.NewSkillHandler(fsSvc)

	r := gin.New()
	r.Use(middleware.TraceMiddleware())
	handler.RegisterRoutes(r, handler.RouteDeps{
		AuthMW:            middleware.AuthMiddleware(integrationTestSecret),
		Login:             func(*gin.Context) {},
		SessionList:       sessH.ListSessions,
		SessionCreate:     sessH.CreateSession,
		SessionMessages:   sessH.GetSessionMessages,
		SendMessage:       sessH.SendMessage,
		WorkspaceList:     wsH.List,
		WorkspaceUpload:   wsH.Upload,
		WorkspaceDownload: wsH.Download,
		WorkspaceContent:  wsH.Content,
		MCPTools:          mcpH.Tools,
		SkillsList:        skillH.List,
	})

	return &testEnv{
		t:         t,
		engine:    r,
		dataDir:   dataDir,
		fsSvc:     fsSvc,
		redisSvc:  redisSvc,
		miniRedis: mr,
		mysqlFake: mysqlFake,
		llm:       llm,
		sessSvc:   sessSvc,
		msgSvc:    msgSvc,
	}
}

// newTestEnvWithRegistry is like newTestEnv but lets the caller supply a
// pre-populated tool registry and a list of tool names exposed to Confuse.
// Useful for integration tests that exercise real tool-call / tool-result
// memory across multiple turns.
func newTestEnvWithRegistry(t *testing.T, llm agent.LLMClient, baseReg *tool.Registry, confuseTools []string) *testEnv {
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

	deps := service.SessionDeps{MySQL: mysqlFake, Redis: redisSvc, FS: fsSvc}
	sessSvc := service.NewSessionService(deps)
	msgSvc := service.NewMessageService(deps, sessSvc.SaveMessage)
	titleSvc := service.NewTitleService(llm, mysqlFake, config.OpenAIConfig{Model: "title-model"})

	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{APIKey: "test", Model: "gpt-test"},
		JWT:    config.JWTConfig{Secret: integrationTestSecret, Expire: "1h"},
		Agents: agentConfigWithTools(confuseTools),
	}
	orch, err := agent.NewOrchestrator(llm, cfg, baseReg, nil, skill.NewLoader("", nil), nil)
	require.NoError(t, err)

	sessH := handler.NewSessionHandler(sessSvc, msgSvc, titleSvc, handler.NewOrchestratorAdapter(orch), dataDir)
	wsH := handler.NewWorkspaceHandler(fsSvc, 1<<20)
	mcpH := handler.NewMCPHandler(baseReg)
	skillH := handler.NewSkillHandler(fsSvc)

	r := gin.New()
	r.Use(middleware.TraceMiddleware())
	handler.RegisterRoutes(r, handler.RouteDeps{
		AuthMW:            middleware.AuthMiddleware(integrationTestSecret),
		Login:             func(*gin.Context) {},
		SessionList:       sessH.ListSessions,
		SessionCreate:     sessH.CreateSession,
		SessionMessages:   sessH.GetSessionMessages,
		SendMessage:       sessH.SendMessage,
		WorkspaceList:     wsH.List,
		WorkspaceUpload:   wsH.Upload,
		WorkspaceDownload: wsH.Download,
		WorkspaceContent:  wsH.Content,
		MCPTools:          mcpH.Tools,
		SkillsList:        skillH.List,
	})

	return &testEnv{
		t:         t,
		engine:    r,
		dataDir:   dataDir,
		fsSvc:     fsSvc,
		redisSvc:  redisSvc,
		miniRedis: mr,
		mysqlFake: mysqlFake,
		llm:       llm,
		sessSvc:   sessSvc,
		msgSvc:    msgSvc,
	}
}

// agentConfigWithTools returns the standard agent config with Confuse granted
// the named tools.
func agentConfigWithTools(confuseTools []string) config.AgentsConfig {
	cfg := agentConfig()
	cfg.Confuse.Tools = confuseTools
	return cfg
}

// agentConfigWithReasoning returns the standard agent config with Confuse
// configured for OpenAI reasoning mode.
func agentConfigWithReasoning() config.AgentsConfig {
	cfg := agentConfig()
	cfg.Confuse.Thinking = true
	cfg.Confuse.ReasoningEffort = "high"
	return cfg
}

// newTestEnvWithAgentsConfig is like newTestEnv but lets the caller supply a
// custom agents configuration. Useful for integration tests that verify
// per-agent settings (e.g., reasoning effort) propagate through the
// orchestrator.
func newTestEnvWithAgentsConfig(t *testing.T, llm agent.LLMClient, agentsCfg config.AgentsConfig) *testEnv {
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

	deps := service.SessionDeps{MySQL: mysqlFake, Redis: redisSvc, FS: fsSvc}
	sessSvc := service.NewSessionService(deps)
	msgSvc := service.NewMessageService(deps, sessSvc.SaveMessage)
	titleSvc := service.NewTitleService(llm, mysqlFake, config.OpenAIConfig{Model: "title-model"})

	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{APIKey: "test", Model: "gpt-test"},
		JWT:    config.JWTConfig{Secret: integrationTestSecret, Expire: "1h"},
		Agents: agentsCfg,
	}
	baseReg := tool.NewRegistry()
	orch, err := agent.NewOrchestrator(llm, cfg, baseReg, nil, skill.NewLoader("", nil), nil)
	require.NoError(t, err)

	sessH := handler.NewSessionHandler(sessSvc, msgSvc, titleSvc, handler.NewOrchestratorAdapter(orch), dataDir)
	wsH := handler.NewWorkspaceHandler(fsSvc, 1<<20)
	mcpH := handler.NewMCPHandler(tool.NewRegistry())
	skillH := handler.NewSkillHandler(fsSvc)

	r := gin.New()
	r.Use(middleware.TraceMiddleware())
	handler.RegisterRoutes(r, handler.RouteDeps{
		AuthMW:            middleware.AuthMiddleware(integrationTestSecret),
		Login:             func(*gin.Context) {},
		SessionList:       sessH.ListSessions,
		SessionCreate:     sessH.CreateSession,
		SessionMessages:   sessH.GetSessionMessages,
		SendMessage:       sessH.SendMessage,
		WorkspaceList:     wsH.List,
		WorkspaceUpload:   wsH.Upload,
		WorkspaceDownload: wsH.Download,
		WorkspaceContent:  wsH.Content,
		MCPTools:          mcpH.Tools,
		SkillsList:        skillH.List,
	})

	return &testEnv{
		t:         t,
		engine:    r,
		dataDir:   dataDir,
		fsSvc:     fsSvc,
		redisSvc:  redisSvc,
		miniRedis: mr,
		mysqlFake: mysqlFake,
		llm:       llm,
		sessSvc:   sessSvc,
		msgSvc:    msgSvc,
	}
}

// authToken returns a Bearer JWT for the default test user that the real auth
// middleware will accept.
func authToken(t *testing.T, userID string) string {
	t.Helper()
	if userID == "" {
		userID = defaultUserID
	}
	tok, err := jwt.Sign(integrationTestSecret, userID, time.Hour)
	require.NoError(t, err)
	return tok
}

// postMessage posts a chat message and returns the recorded SSE body. The
// caller owns inspecting the body.
func (e *testEnv) postMessage(body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/"+defaultSessionID+"/messages",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	return w
}

// parseSSEBody splits an SSE body into (eventTypes, payloads) preserving
// order. Each block is "event: <type>\ndata: <json>\n\n".
func parseSSEBody(t *testing.T, body string) ([]string, []map[string]any) {
	t.Helper()
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, nil
	}
	types := make([]string, 0)
	payloads := make([]map[string]any, 0)
	for b := range strings.SplitSeq(body, "\n\n") {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		var eventType string
		var dataLine string
		for line := range strings.SplitSeq(b, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				eventType = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				dataLine = strings.TrimPrefix(line, "data: ")
			}
		}
		require.NotEmpty(t, eventType, "block missing event: line: %q", b)
		require.NotEmpty(t, dataLine, "block missing data: line: %q", b)
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(dataLine), &payload), "block %q", b)
		types = append(types, eventType)
		payloads = append(payloads, payload)
	}
	return types, payloads
}

// requireEventSequence asserts the SSE event types contain the wanted subsequence
// (not necessarily contiguous). Each wanted type must appear in order.
func requireEventSubsequence(t *testing.T, got, want []string) {
	t.Helper()
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	require.Equal(t, len(want), i, "expected SSE subsequence %v within %v", want, got)
}

// requireEventPresent asserts that evt appears at least once in events.
func requireEventPresent(t *testing.T, events []string, evt string) {
	t.Helper()
	var n int
	for _, e := range events {
		if e == evt {
			n++
		}
	}
	require.Greaterf(t, n, 0, "expected SSE event %q in %v", evt, events)
}
