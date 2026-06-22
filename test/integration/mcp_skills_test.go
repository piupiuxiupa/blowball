package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/handler"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/store/fs"
	redisstore "github.com/lush/blowball/internal/store/redis"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/skill"
)

// scriptedLLM is a minimal agent.LLMClient that returns canned responses.
type scriptedLLM struct {
	mu        sync.Mutex
	responses []agent.LLMResponse
	calls     int
	lastReq   agent.LLMRequest
	requests  []agent.LLMRequest
}

func (s *scriptedLLM) StreamChat(ctx context.Context, req agent.LLMRequest, onToken func(string) error, onReasoning func(string) error) (agent.LLMResponse, error) {
	s.mu.Lock()
	s.lastReq = req
	s.requests = append(s.requests, req)
	if s.calls >= len(s.responses) {
		s.mu.Unlock()
		return agent.LLMResponse{FinishReason: "stop", Content: "done"}, nil
	}
	r := s.responses[s.calls]
	s.calls++
	s.mu.Unlock()
	for _, t := range r.Content {
		_ = onToken(string(t))
	}
	return r, nil
}

func (s *scriptedLLM) confuseRequest() agent.LLMRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.requests) - 1; i >= 0; i-- {
		if len(s.requests[i].Messages) > 0 && s.requests[i].Messages[0].Role == "system" {
			content := s.requests[i].Messages[0].Content
			if contains(content, "you are confuse") {
				return s.requests[i]
			}
		}
	}
	return s.lastReq
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// TestIntegration_AgentMCPToolVisibility verifies that an agent with MCP
// configuration receives only the allowed tools in its system prompt and OpenAI
// tools list.
func TestIntegration_AgentMCPToolVisibility(t *testing.T) {
	baseReg := tool.NewRegistry()
	require.NoError(t, baseReg.Register(&tool.ToolSpec{
		Name:        "remote_search",
		Description: "search the web",
		Execute:     func(ctx context.Context, args json.RawMessage) (any, error) { return "", nil },
	}))
	require.NoError(t, baseReg.Register(&tool.ToolSpec{
		Name:        "remote_fetch",
		Description: "fetch a url",
		Execute:     func(ctx context.Context, args json.RawMessage) (any, error) { return "", nil },
	}))

	llm := &scriptedLLM{
		responses: []agent.LLMResponse{{
			FinishReason: "stop",
			Content:      "done",
			Usage:        agent.Usage{TotalTokens: 1},
		}},
	}

	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{APIKey: "test", Model: "gpt-test"},
		JWT:    config.JWTConfig{Secret: integrationTestSecret, Expire: "1h"},
		Agents: config.AgentsConfig{
			Confuse: config.AgentConfig{
				Name:         stream.AgentConfuse,
				Model:        "gpt-test",
				SystemPrompt: "you are confuse",
				MaxTokens:    256,
				MCP: config.AgentMCPConfig{
					Servers: []config.AgentMCPServerConfig{{
						Name:  "remote",
						Tools: []string{"remote_search"},
					}},
				},
			},
			Chongzhi: config.AgentConfig{
				Name:         stream.AgentChongzhi,
				Model:        "gpt-test",
				SystemPrompt: "you are chongzhi",
				MaxTokens:    256,
				Tools:        []string{"xizhi_write_file"},
			},
			Liang: config.AgentConfig{
				Name:         stream.AgentLiang,
				Model:        "gpt-test",
				SystemPrompt: "you are liang",
				MaxTokens:    256,
			},
		},
	}

	srv, mysqlFake := setupMCPIntegrationServer(t, llm, cfg, baseReg, map[string][]string{"remote": {"remote_search", "remote_fetch"}}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-mcp/messages", jsonBody(t, map[string]string{"content": "hi"}))
	req.Header.Set("Authorization", "Bearer "+authToken(t, defaultUserID))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Wait for the async batch save to finish so the temp data directory can be
	// cleaned up without racing the FS writer.
	require.Eventually(t, func() bool {
		return len(mysqlFake.messagesFor("sess-mcp")) >= 4
	}, 2*time.Second, 10*time.Millisecond, "expected sess-mcp messages to be persisted")

	require.NotNil(t, llm.confuseRequest().Tools)
	var tools []struct {
		Function struct{ Name string `json:"name"` } `json:"function"`
	}
	require.NoError(t, json.Unmarshal(llm.confuseRequest().Tools, &tools))
	names := make(map[string]bool, len(tools))
	for _, t2 := range tools {
		names[t2.Function.Name] = true
	}
	assert.True(t, names["remote_search"])
	assert.False(t, names["remote_fetch"])

	prompt := llm.confuseRequest().Messages[0].Content
	assert.Contains(t, prompt, "remote_search")
	assert.NotContains(t, prompt, "remote_fetch")
}

// TestIntegration_AgentSkillCatalog verifies that an agent with skills receives
// a skill catalog in its system prompt and can invoke read_skill.
func TestIntegration_AgentSkillCatalog(t *testing.T) {
	skillDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(skillDir, "coding-style"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "coding-style", "SKILL.md"), []byte("---\nname: coding-style\ndescription: Coding conventions\n---\n# Style\n"), 0o644))

	baseReg := tool.NewRegistry()
	loader := skill.NewLoader(skillDir, nil)
	require.NoError(t, skill.RegisterReadSkill(baseReg, loader))

	llm := &scriptedLLM{
		responses: []agent.LLMResponse{{
			FinishReason: "tool_calls",
			ToolCalls: []agent.ToolCall{{
				ID:       "tc_1",
				Function: agent.ToolCallFunction{Name: "read_skill", Arguments: `{"name":"coding-style"}`},
			}},
			Usage: agent.Usage{TotalTokens: 1},
		}, {
			FinishReason: "stop",
			Content:      "done",
			Usage:        agent.Usage{TotalTokens: 1},
		}},
	}

	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{APIKey: "test", Model: "gpt-test"},
		JWT:    config.JWTConfig{Secret: integrationTestSecret, Expire: "1h"},
		Agents: config.AgentsConfig{
			Confuse: config.AgentConfig{
				Name:         stream.AgentConfuse,
				Model:        "gpt-test",
				SystemPrompt: "you are confuse",
				MaxTokens:    256,
				Skills:       []string{"coding-style"},
			},
			Chongzhi: config.AgentConfig{
				Name:         stream.AgentChongzhi,
				Model:        "gpt-test",
				SystemPrompt: "you are chongzhi",
				MaxTokens:    256,
				Tools:        []string{"xizhi_write_file"},
			},
			Liang: config.AgentConfig{
				Name:         stream.AgentLiang,
				Model:        "gpt-test",
				SystemPrompt: "you are liang",
				MaxTokens:    256,
			},
		},
	}

	srv, mysqlFake := setupMCPIntegrationServer(t, llm, cfg, baseReg, nil, loader)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-skills/messages", jsonBody(t, map[string]string{"content": "hi"}))
	req.Header.Set("Authorization", "Bearer "+authToken(t, defaultUserID))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Wait for the async batch save to finish so the temp data directory can be
	// cleaned up without racing the FS writer.
	require.Eventually(t, func() bool {
		return len(mysqlFake.messagesFor("sess-skills")) >= 4
	}, 2*time.Second, 10*time.Millisecond, "expected sess-skills messages to be persisted")

	prompt := llm.confuseRequest().Messages[0].Content
	assert.Contains(t, prompt, "coding-style")
	assert.Contains(t, prompt, "Coding conventions")
	assert.Contains(t, prompt, "read_skill")

	// Second LLM round carries the read_skill result.
	req2 := llm.confuseRequest()
	require.GreaterOrEqual(t, len(req2.Messages), 3)
	last := req2.Messages[len(req2.Messages)-1]
	assert.Equal(t, "tool", last.Role)
	assert.Contains(t, last.Content, "# Style")
}

func setupMCPIntegrationServer(t *testing.T, llm agent.LLMClient, cfg *config.Config, baseReg *tool.Registry, serverTools map[string][]string, loader *skill.Loader) (*gin.Engine, *memoryMySQL) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dataDir := t.TempDir()
	fsSvc, err := fs.New(dataDir)
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	redisSvc, err := redisstore.New(mr.Addr(), "", 0, time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisSvc.Close() })

	mysqlFake := newMemoryMySQL()
	require.NoError(t, mysqlFake.CreateSession(context.Background(), model.Session{SessionID: "sess-mcp", UserID: defaultUserID, TraceID: "trace-mcp"}))
	require.NoError(t, mysqlFake.CreateSession(context.Background(), model.Session{SessionID: "sess-skills", UserID: defaultUserID, TraceID: "trace-skills"}))

	deps := service.SessionDeps{MySQL: mysqlFake, Redis: redisSvc, FS: fsSvc}
	sessSvc := service.NewSessionService(deps)
	msgSvc := service.NewMessageService(deps, sessSvc.SaveMessage)
	titleSvc := service.NewTitleService(llm, mysqlFake, config.OpenAIConfig{Model: "title-model"})

	orch, err := agent.NewOrchestrator(llm, cfg, baseReg, serverTools, loader, nil)
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
	return r, mysqlFake
}

func jsonBody(t *testing.T, v any) *strings.Reader {
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return strings.NewReader(string(b))
}
