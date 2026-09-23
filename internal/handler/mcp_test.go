package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/mcpmarket"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/mcp"
	"github.com/lush/blowball/internal/tool/mcpclient"
	"github.com/lush/blowball/internal/tool/xizhi"
)

// allXizhiReg builds a registry with every Xizhi tool enabled (scoped to a
// temp dir) plus the synthetic invoke_* tools, so tests can assert they are
// EXCLUDED from the MCP-only catalogue.
func allXizhiReg(t *testing.T) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry()
	xizhi.RegisterAll(reg, t.TempDir(), config.XizhiConfig{
		Read:      config.XizhiToolConfig{Enabled: true},
		Write:     config.XizhiToolConfig{Enabled: true},
		Modify:    config.XizhiToolConfig{Enabled: true},
		ListFiles: config.XizhiToolConfig{Enabled: true},
		Tree:      config.XizhiToolConfig{Enabled: true},
		Find:      config.XizhiToolConfig{Enabled: true},
	})
	return reg
}

// mcpToolResp is the response shape asserted by the handler tests.
type mcpToolResp struct {
	Tools []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Server      string          `json:"server"`
	} `json:"tools"`
}

// doTools builds a router with the given handler, optionally injecting a
// userID into the gin context (simulating AuthMiddleware), and issues a GET.
func doTools(t *testing.T, h *MCPHandler, withUser bool) mcpToolResp {
	t.Helper()
	r := gin.New()
	if withUser {
		r.Use(func(c *gin.Context) { c.Set(middleware.UserIDKey, "u-1"); c.Next() })
	}
	r.GET("/api/v1/mcp/tools", h.Tools)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/tools", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp mcpToolResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

// TestMCPTools_ExcludesBuiltins verifies that built-in tools (Xizhi) and the
// synthetic invoke_* tools are no longer returned: the endpoint lists only
// MCP-sourced tools.
func TestMCPTools_ExcludesBuiltins(t *testing.T) {
	reg := allXizhiReg(t)
	h := NewMCPHandler(reg, nil, nil, nil)

	resp := doTools(t, h, false)
	assert.Empty(t, resp.Tools, "no built-in (xizhi/invoke) tools should be returned")

	names := make(map[string]bool, len(resp.Tools))
	for _, t2 := range resp.Tools {
		names[t2.Name] = true
	}
	assert.False(t, names[xizhi.NameReadFile])
	assert.False(t, names["spawn_subagent"])
}

// TestMCPTools_OperatorProxyIncludedWithSource verifies that operator (global)
// MCP proxy tools appear with their owning server in the `server` field.
func TestMCPTools_OperatorProxyIncludedWithSource(t *testing.T) {
	reg := tool.NewRegistry()
	mt := &mockMCPTransport{
		initResult: &mcpclient.InitializeResult{ProtocolVersion: "2024-11-05"},
		tools: []mcpclient.Tool{
			{Name: "remote_add", Description: "adds remotely", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	old := mcpclient.TransportFactory
	mcpclient.TransportFactory = func(sc config.MCPServerConfig) (mcpclient.Transport, error) {
		return mt, nil
	}
	defer func() { mcpclient.TransportFactory = old }()

	mgr, err := mcpclient.RegisterAllWithManager(context.Background(), reg, config.MCPConfig{
		Servers: []config.MCPServerConfig{{
			Name:      "remote",
			Transport: "sse",
			URL:       "http://localhost:3001/sse",
		}},
	})
	require.NoError(t, err)
	defer mgr.Close()

	h := NewMCPHandler(reg, mgr.ServerTools(), nil, nil)
	resp := doTools(t, h, false)

	require.Len(t, resp.Tools, 1)
	assert.Equal(t, "remote_add", resp.Tools[0].Name)
	assert.Equal(t, "adds remotely", resp.Tools[0].Description)
	assert.Equal(t, "function", resp.Tools[0].Type)
	assert.Equal(t, "remote", resp.Tools[0].Server, "operator proxy tool must be attributed to its server")
	assert.True(t, json.Valid(resp.Tools[0].Parameters))
}

// TestMCPTools_PerUserCachedToolsIncludedWithSource verifies that the caller's
// per-user MCP cached tools appear with their server in the `server` field,
// read from the workspace config cache with no network.
func TestMCPTools_PerUserCachedToolsIncludedWithSource(t *testing.T) {
	reg := tool.NewRegistry() // no operator MCP
	ws := t.TempDir()
	require.NoError(t, mcp.WriteServer(ws, mcp.Server{
		Name:      "github",
		URL:       "http://gh/mcp",
		Transport: "http",
		Tools: []mcp.ToolCache{{
			Name:        "create_issue",
			Description: "open an issue",
			InputSchema: json.RawMessage(`{"type":"object","required":["title"]}`),
		}},
	}))

	h := NewMCPHandler(reg, nil, func(userID string) string { return ws }, nil)
	resp := doTools(t, h, true)

	require.Len(t, resp.Tools, 1)
	assert.Equal(t, "create_issue", resp.Tools[0].Name)
	assert.Equal(t, "open an issue", resp.Tools[0].Description)
	assert.Equal(t, "github", resp.Tools[0].Server, "per-user tool must be attributed to its server")
}

// TestMCPTools_MissingUserConfigYieldsOnlyGlobal verifies that a missing/empty
// per-user config does not error and still returns the operator tools.
func TestMCPTools_MissingUserConfigYieldsOnlyGlobal(t *testing.T) {
	reg := tool.NewRegistry()
	mt := &mockMCPTransport{
		initResult: &mcpclient.InitializeResult{ProtocolVersion: "2024-11-05"},
		tools:      []mcpclient.Tool{{Name: "remote_add", Description: "adds"}},
	}
	old := mcpclient.TransportFactory
	mcpclient.TransportFactory = func(sc config.MCPServerConfig) (mcpclient.Transport, error) { return mt, nil }
	defer func() { mcpclient.TransportFactory = old }()
	mgr, err := mcpclient.RegisterAllWithManager(context.Background(), reg, config.MCPConfig{
		Servers: []config.MCPServerConfig{{Name: "remote", Transport: "sse", URL: "http://x/sse"}},
	})
	require.NoError(t, err)
	defer mgr.Close()

	h := NewMCPHandler(reg, mgr.ServerTools(), func(userID string) string { return t.TempDir() }, nil) // empty workspace
	resp := doTools(t, h, true)

	require.Len(t, resp.Tools, 1, "missing user config must yield only the global tool")
	assert.Equal(t, "remote_add", resp.Tools[0].Name)
}

// TestMCPTools_MalformedPerUserServerOmitted verifies that a single malformed
// per-user server cache is omitted while the rest (and global tools) still
// return, without failing the whole endpoint.
func TestMCPTools_MalformedPerUserServerOmitted(t *testing.T) {
	reg := tool.NewRegistry()
	ws := t.TempDir()
	// A good server with cached tools.
	require.NoError(t, mcp.WriteServer(ws, mcp.Server{
		Name: "good", URL: "http://g/mcp", Transport: "http",
		Tools: []mcp.ToolCache{{Name: "good_tool", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}))
	// A malformed server (invalid JSON) written directly to bypass validation.
	require.NoError(t, os.MkdirAll(filepath.Join(ws, ".blowball", "mcp", "bad"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, ".blowball", "mcp", "bad", "config.json"), []byte("{not json"), 0o644))

	h := NewMCPHandler(reg, nil, func(userID string) string { return ws }, nil)
	resp := doTools(t, h, true)

	require.Len(t, resp.Tools, 1, "malformed server must be omitted, good server still returned")
	assert.Equal(t, "good_tool", resp.Tools[0].Name)
}

// mockMCPTransport is a minimal Transport double for handler tests.
type mockMCPTransport struct {
	initResult *mcpclient.InitializeResult
	initErr    error
	tools      []mcpclient.Tool
	listErr    error
	callResult *mcpclient.ToolsCallResult
	callErr    error
	closed     bool
}

func (m *mockMCPTransport) Initialize(ctx context.Context, params mcpclient.InitializeParams) (*mcpclient.InitializeResult, error) {
	if m.initErr != nil {
		return nil, m.initErr
	}
	return m.initResult, nil
}

func (m *mockMCPTransport) ListTools(ctx context.Context) (*mcpclient.ToolsListResult, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return &mcpclient.ToolsListResult{Tools: m.tools}, nil
}

func (m *mockMCPTransport) CallTool(ctx context.Context, params mcpclient.ToolsCallParams) (*mcpclient.ToolsCallResult, error) {
	if m.callErr != nil {
		return nil, m.callErr
	}
	return m.callResult, nil
}

func (m *mockMCPTransport) Close() error {
	m.closed = true
	return nil
}

// TestMCPTools_MarketCachedToolsMergedWithShadowing verifies the mcp-market
// merge on the tools endpoint: market servers' cached tools appear attributed
// to the market server name, a workspace server of the same name shadows the
// market entry, and the endpoint still makes no MCP connections.
func TestMCPTools_MarketCachedToolsMergedWithShadowing(t *testing.T) {
	reg := tool.NewRegistry()
	ws := t.TempDir()
	require.NoError(t, mcp.WriteServer(ws, mcp.Server{
		Name: "shared", URL: "http://ws/mcp", Transport: "http",
		Tools: []mcp.ToolCache{{Name: "ws_tool"}},
	}))

	// Fake allowlist service + operator-synced market payloads.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"servers": []map[string]string{
				{"name": "marketed", "path": "mid1/marketed"},
				{"name": "shared", "path": "mid1/shared"},
			},
		})
	}))
	defer srv.Close()
	root := t.TempDir()
	for _, name := range []string{"marketed", "shared"} {
		dir := filepath.Join(root, "mid1", name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		body := `{"url":"http://` + name + `/mcp","transport":"http","tools":[{"name":"` + name + `_tool"}]}`
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644))
	}
	market := mcpmarket.New(config.MCPMarketConfig{URL: srv.URL, Timeout: 2 * time.Second}, root)
	require.NotNil(t, market)

	h := NewMCPHandler(reg, nil, func(userID string) string { return ws }, market)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(middleware.UserIDKey, "u-1"); c.Next() })
	r.GET("/api/v1/mcp/tools", h.Tools)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/tools", nil)
	req.Header.Set("Authorization", "Bearer jwt-u1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp mcpToolResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Tools, 2)
	got := map[string]string{}
	for _, t2 := range resp.Tools {
		got[t2.Name] = t2.Server
	}
	assert.Equal(t, "shared", got["ws_tool"], "workspace server wins the shadowed name")
	assert.Equal(t, "marketed", got["marketed_tool"], "market tool attributed to its market server")
}
