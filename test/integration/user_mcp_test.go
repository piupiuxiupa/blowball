package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
)

// jsonrpcRequest is the JSON-RPC envelope a Streamable HTTP MCP server receives.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// startFakeMCPServer launches a minimal Streamable HTTP MCP server that
// advertises an "echo" tool and returns its `msg` argument verbatim. It records
// the Authorization header on every request so the test can assert the caller's
// secret was injected server-side (and, transitively, that the agent never saw
// it).
func startFakeMCPServer(t *testing.T) *fakeMCPRecorder {
	t.Helper()
	rec := &fakeMCPRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.authHeaders = append(rec.authHeaders, r.Header.Get("Authorization"))
		rec.mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		var req jsonrpcRequest
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "fake-session")
			writeJSONRPCResult(t, w, req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake", "version": "0.1"},
			})
		case "tools/list":
			writeJSONRPCResult(t, w, req.ID, map[string]any{"tools": []any{
				map[string]any{
					"name":        "echo",
					"description": "echo the msg",
					"inputSchema": map[string]any{
						"type": "object",
						"required": []string{"msg"},
						"properties": map[string]any{
							"msg": map[string]any{"type": "string"},
						},
					},
				},
			}})
		case "tools/call":
			var params struct {
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			msg, _ := params.Arguments["msg"].(string)
			writeJSONRPCResult(t, w, req.ID, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "echo:" + msg}},
			})
		default:
			t.Errorf("fake mcp server: unexpected method %q", req.Method)
		}
	}))
	t.Cleanup(srv.Close)
	rec.server = srv
	return rec
}

type fakeMCPRecorder struct {
	server     *httptest.Server
	mu         sync.Mutex
	authHeaders []string
}

func (f *fakeMCPRecorder) url() string { return f.server.URL }
func (f *fakeMCPRecorder) auths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.authHeaders))
	copy(out, f.authHeaders)
	return out
}

func writeJSONRPCResult(t *testing.T, w http.ResponseWriter, id int, result any) {
	t.Helper()
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	resp := map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(raw)}
	require.NoError(t, json.NewEncoder(w).Encode(resp))
}

// TestIntegration_UserMCPFullTurn exercises the full per-user MCP path through
// the real orchestrator: the agent calls mcp_add_server (pointing at a fake
// MCP server), then mcp_call, and the result flows back into the conversation.
// It also asserts (a) credentials are never echoed into model-visible text and
// (b) the caller's auth was injected server-side.
func TestIntegration_UserMCPFullTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)

	fake := startFakeMCPServer(t)

	// Round 1: add the server. Round 2: call echo. Round 3: stop.
	const secret = "bearer-secret-integration"
	llm := &scriptedLLM{responses: []agent.LLMResponse{
		{
			FinishReason: "tool_calls",
			ToolCalls: []agent.ToolCall{{
				ID:       "tc_add",
				Function: agent.ToolCallFunction{Name: "mcp_add_server", Arguments: `{"name":"fake","url":"` + fake.url() + `","description":"fake echo","transport":"http","auth":{"type":"bearer","value":"` + secret + `"}}`},
			}},
			Usage: agent.Usage{TotalTokens: 1},
		},
		{
			FinishReason: "tool_calls",
			ToolCalls: []agent.ToolCall{{
				ID:       "tc_call",
				Function: agent.ToolCallFunction{Name: "mcp_call", Arguments: `{"server":"fake","tool":"echo","args":{"msg":"hi"}}`},
			}},
			Usage: agent.Usage{TotalTokens: 1},
		},
		{
			FinishReason: "stop",
			Content:      "got echo:hi",
			Usage:        agent.Usage{TotalTokens: 1},
		},
	}}

	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{APIKey: "test", Model: "gpt-test"},
		JWT:    config.JWTConfig{Secret: integrationTestSecret, Expire: "1h"},
		Agents: config.AgentsConfig{
			Confucius: config.AgentConfig{
				Name:         stream.AgentConfucius,
				Model:        "gpt-test",
				SystemPrompt: "you are confucius",
				MaxTokens:    512,
				Tools:        []string{"mcp_add_server", "mcp_call", "mcp_list_servers", "mcp_remove_server"},
			},
			Chongzhi: config.AgentConfig{Name: stream.AgentChongzhi, Model: "gpt-test", SystemPrompt: "you are chongzhi", MaxTokens: 256},
			Liang:    config.AgentConfig{Name: stream.AgentLiang, Model: "gpt-test", SystemPrompt: "you are liang", MaxTokens: 256},
		},
	}

	dataDir := t.TempDir()
	fsSvc, err := fs.New(dataDir)
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	redisSvc, err := redisstore.New(mr.Addr(), "", 0, time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisSvc.Close() })

	mysqlFake := newMemoryMySQL()
	require.NoError(t, mysqlFake.CreateSession(context.Background(), model.Session{SessionID: "sess-umcp", UserID: defaultUserID, TraceID: "trace-umcp"}))

	deps := service.SessionDeps{MySQL: mysqlFake, Redis: redisSvc, FS: fsSvc}
	sessSvc := service.NewSessionService(deps)
	msgSvc := service.NewMessageService(deps, sessSvc.SaveMessage)
	titleSvc := service.NewTitleService(llm, mysqlFake, config.OpenAIConfig{Model: "title-model"})

	baseReg := tool.NewRegistry()
	orch, err := agent.NewOrchestrator(llm, cfg, baseReg, nil, nil, nil)
	require.NoError(t, err)

	streamH := handler.NewMessageStreamHandler(sessSvc, msgSvc, titleSvc, handler.NewOrchestratorAdapter(orch), dataDir)

	r := gin.New()
	r.Use(middleware.TraceMiddleware())
	handler.RegisterRoutes(r, handler.RouteDeps{
		AuthMW:       middleware.AuthMiddleware(integrationTestSecret),
		SendMessage:  streamH.SendMessage,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-umcp/messages", jsonBody(t, map[string]string{"content": "echo hi for me"}))
	req.Header.Set("Authorization", "Bearer "+authToken(t, defaultUserID))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	body := w.Body.String()

	// Parse the SSE stream to inspect tool_result events specifically.
	eventTypes, payloads := parseSSEBody(t, body)

	// The mcp_call tool_result carries the remote echo result.
	var callResultSeen, addResultSeen bool
	for i, evt := range eventTypes {
		if evt != "tool_result" {
			continue
		}
		content, _ := payloads[i]["content"].(string)
		// Leak invariant #1: tool RESULT output (ours) never echoes the secret,
		// even though the model supplied it as input on the mcp_add_server call.
		assert.NotContains(t, content, secret, "tool_result leaked the credential")
		if strings.Contains(content, `"status":"added"`) {
			addResultSeen = true
			assert.Contains(t, content, `"value":"***"`, "add result must redact the credential")
		}
		if strings.Contains(content, "echo:hi") {
			callResultSeen = true
		}
	}
	assert.True(t, addResultSeen, "expected the mcp_add_server tool_result")
	assert.True(t, callResultSeen, "expected the mcp_call tool_result to carry echo:hi")

	// The final assistant content is assembled server-side and persisted; verify
	// it there rather than across tokenized SSE events.
	require.Eventually(t, func() bool {
		return len(mysqlFake.messagesFor("sess-umcp")) > 0
	}, 2*time.Second, 10*time.Millisecond, "expected sess-umcp messages to be persisted")
	var sawAssistantEcho bool
	for _, m := range mysqlFake.messagesFor("sess-umcp") {
		if m.Role == "assistant" && strings.Contains(m.Content, "echo:hi") {
			sawAssistantEcho = true
		}
	}
	assert.True(t, sawAssistantEcho, "expected the final assistant message to reference echo:hi")

	// Server-side auth injection: the fake MCP server received the bearer
	// header on its calls.
	auths := fake.auths()
	require.NotEmpty(t, auths, "expected the fake server to receive at least one authenticated request")
	for _, a := range auths {
		assert.Equal(t, "Bearer "+secret, a)
	}
}
