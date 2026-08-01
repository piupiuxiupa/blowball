package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/tool/mcpclient"
)

// fakeTransport is a test double for mcpclient.Transport that counts calls so
// tests can assert connection reuse and timeout behavior.
type fakeTransport struct {
	mu         sync.Mutex
	initCount  int
	listCount  int
	callCount  int
	tools      []mcpclient.Tool
	callResult *mcpclient.ToolsCallResult
	callErr    error
	initDelay  time.Duration
	listDelay  time.Duration
	callDelay  time.Duration
	closed     bool
}

func (f *fakeTransport) Initialize(ctx context.Context, _ mcpclient.InitializeParams) (*mcpclient.InitializeResult, error) {
	f.mu.Lock()
	f.initCount++
	delay := f.initDelay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &mcpclient.InitializeResult{ProtocolVersion: "2024-11-05"}, nil
}

func (f *fakeTransport) ListTools(ctx context.Context) (*mcpclient.ToolsListResult, error) {
	f.mu.Lock()
	f.listCount++
	delay := f.listDelay
	tools := f.tools
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &mcpclient.ToolsListResult{Tools: tools}, nil
}

func (f *fakeTransport) CallTool(ctx context.Context, _ mcpclient.ToolsCallParams) (*mcpclient.ToolsCallResult, error) {
	f.mu.Lock()
	f.callCount++
	delay := f.callDelay
	callErr := f.callErr
	callResult := f.callResult
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if callErr != nil {
		return nil, callErr
	}
	if callResult != nil {
		return callResult, nil
	}
	return &mcpclient.ToolsCallResult{Content: []mcpclient.Content{{Type: "text", Text: "ok"}}}, nil
}

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeTransport) snapshot() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.initCount, f.listCount, f.callCount
}

// newManagerWithFake builds a Manager over a temp workspace whose
// transportFactory returns ft. The fake is shared so the factory call count
// reveals whether the manager reconnected.
func newManagerWithFake(t *testing.T, ft *fakeTransport, opts ManagerOptions) (*Manager, string) {
	t.Helper()
	ws := t.TempDir()
	opts.WorkspaceRoot = ws
	factoryCalls := 0
	_ = factoryCalls
	opts.TransportFactory = func(server Server, _ TimeoutConfig) (mcpclient.Transport, error) {
		return ft, nil
	}
	return NewManager(opts), ws
}

// ---------------------------------------------------------------------------
// Config: load / validate / write (tasks 1.2, 1.3, 1.4)
// ---------------------------------------------------------------------------

func TestLoadConfig_MissingFileIsEmpty(t *testing.T) {
	cfg, err := LoadConfig(t.TempDir())
	require.NoError(t, err)
	require.Empty(t, cfg.Servers)
}

func TestLoadConfig_Valid(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, configDir), 0o755))
	body := `{"servers":[{"name":"calc","url":"http://x/mcp","transport":"http","auth":{"type":"bearer","value":"tok"}}]}`
	require.NoError(t, os.WriteFile(ConfigPath(ws), []byte(body), 0o644))

	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, cfg.Servers, 1)
	assert.Equal(t, "calc", cfg.Servers[0].Name)
	assert.Equal(t, AuthBearer, cfg.Servers[0].Auth.Type)
}

func TestLoadConfig_BareArrayForm(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, configDir), 0o755))
	body := `[{"name":"calc","url":"http://x/mcp","transport":"http"}]`
	require.NoError(t, os.WriteFile(ConfigPath(ws), []byte(body), 0o644))
	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, cfg.Servers, 1)
}

func TestLoadConfig_MalformedReturnsError(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, configDir), 0o755))
	require.NoError(t, os.WriteFile(ConfigPath(ws), []byte("{not json"), 0o644))
	_, err := LoadConfig(ws)
	require.Error(t, err)
}

func TestValidate_RejectsNonHTTP(t *testing.T) {
	c := &Config{Servers: []Server{{Name: "s", URL: "http://x", Transport: "stdio"}}}
	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only transport \"http\"")
}

func TestValidate_RejectsOAuth(t *testing.T) {
	c := &Config{Servers: []Server{{Name: "s", URL: "http://x", Transport: "http", Auth: Auth{Type: "oauth"}}}}
	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OAuth")
}

func TestValidate_RejectsDuplicateName(t *testing.T) {
	c := &Config{Servers: []Server{
		{Name: "s", URL: "http://x", Transport: "http"},
		{Name: "s", URL: "http://y", Transport: "http"},
	}}
	require.Error(t, c.Validate())
}

func TestValidate_RequiresNameAndURL(t *testing.T) {
	require.Error(t, (&Config{Servers: []Server{{URL: "http://x", Transport: "http"}}}).Validate())
	require.Error(t, (&Config{Servers: []Server{{Name: "s", Transport: "http"}}}).Validate())
}

func TestWriteConfig_AtomicAndRoundTrips(t *testing.T) {
	ws := t.TempDir()
	c := &Config{Servers: []Server{{Name: "calc", URL: "http://x/mcp", Transport: "http", Auth: Auth{Type: AuthBearer, Value: "tok"}, Tools: []ToolCache{{Name: "add", InputSchema: json.RawMessage(`{"type":"object"}`)}}}}}
	require.NoError(t, WriteConfig(ws, c))

	// No leftover temp files.
	entries, err := os.ReadDir(filepath.Join(ws, configDir))
	require.NoError(t, err)
	assert.Len(t, entries, 1)

	loaded, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, loaded.Servers, 1)
	assert.Equal(t, "tok", loaded.Servers[0].Auth.Value)
	require.Len(t, loaded.Servers[0].Tools, 1)
}

func TestAddServer_DuplicateRejected(t *testing.T) {
	c := &Config{Servers: []Server{{Name: "s", URL: "http://x", Transport: "http"}}}
	err := c.AddServer(Server{Name: "s", URL: "http://y"})
	require.Error(t, err)
	assert.Len(t, c.Servers, 1, "duplicate must not mutate")
}

func TestRemoveServer_PreservesOthers(t *testing.T) {
	c := &Config{Servers: []Server{{Name: "a", URL: "http://a", Transport: "http"}, {Name: "b", URL: "http://b", Transport: "http"}}}
	assert.True(t, c.RemoveServer("a"))
	require.Len(t, c.Servers, 1)
	assert.Equal(t, "b", c.Servers[0].Name)
	assert.False(t, c.RemoveServer("missing"))
}

// ---------------------------------------------------------------------------
// Turn-scoped connection reuse + timeouts (tasks 2.1, 2.2)
// ---------------------------------------------------------------------------

func TestManager_ConnectionReuseWithinTurn(t *testing.T) {
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add"}}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer m.Close()
	require.NoError(t, WriteConfig(ws, &Config{Servers: []Server{{Name: "calc", URL: "http://x", Transport: "http"}}}))

	ctx := context.Background()
	c1, err := m.Conn(ctx, "calc")
	require.NoError(t, err)
	c2, err := m.Conn(ctx, "calc")
	require.NoError(t, err)
	assert.Same(t, c1, c2, "second Conn must reuse the cached connection")

	ini, _, _ := ft.snapshot()
	assert.Equal(t, 1, ini, "initialize must run exactly once within a turn")
}

func TestManager_ConnectTimeoutAvoidsHandshakeHang(t *testing.T) {
	ft := &fakeTransport{initDelay: 200 * time.Millisecond}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: 20 * time.Millisecond, CallTimeout: time.Second})
	defer m.Close()
	require.NoError(t, WriteConfig(ws, &Config{Servers: []Server{{Name: "calc", URL: "http://x", Transport: "http"}}}))

	_, err := m.Conn(context.Background(), "calc")
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "want DeadlineExceeded, got %v", err)
}

func TestManager_TotalCallTimeout(t *testing.T) {
	ft := &fakeTransport{
		tools:     []mcpclient.Tool{{Name: "add", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		callDelay: 300 * time.Millisecond,
	}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: 40 * time.Millisecond})
	defer m.Close()
	require.NoError(t, WriteConfig(ws, &Config{Servers: []Server{{
		Name: "calc", URL: "http://x", Transport: "http",
		Tools: []ToolCache{{Name: "add", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}}}))

	// callTool wraps the whole op in the total-call timeout; a slow call must
	// surface a deadline error.
	_, err := callTool(context.Background(), m, "calc", "add", json.RawMessage(`{}`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "want DeadlineExceeded, got %v", err)
}

func TestManager_CloseDropsConnections(t *testing.T) {
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add"}}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{})
	require.NoError(t, WriteConfig(ws, &Config{Servers: []Server{{Name: "calc", URL: "http://x", Transport: "http"}}}))
	_, err := m.Conn(context.Background(), "calc")
	require.NoError(t, err)
	require.NoError(t, m.Close())
	assert.True(t, ft.closed)
	// After Close, no further connections.
	_, err = m.Conn(context.Background(), "calc")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Schema validation (task 4.2)
// ---------------------------------------------------------------------------

func TestValidateArgs_Required(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["x"],"properties":{"x":{"type":"string"}}}`)
	require.NoError(t, validateArgs(schema, json.RawMessage(`{"x":"hi"}`)))
	err := validateArgs(schema, json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required field")
}

func TestValidateArgs_TypeMismatch(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`)
	err := validateArgs(schema, json.RawMessage(`{"n":"not-int"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected type")
}

func TestValidateArgs_EmptySchemaAccepts(t *testing.T) {
	require.NoError(t, validateArgs(nil, json.RawMessage(`{"anything":1}`)))
}

// ---------------------------------------------------------------------------
// mcp_* tools: list / add / remove / call (tasks 3.1-3.4, 4.1, 4.2, 4.3)
// ---------------------------------------------------------------------------

func TestListServers_RedactsCredentials(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, WriteConfig(ws, &Config{Servers: []Server{
		{Name: "a", URL: "http://a", Transport: "http", Auth: Auth{Type: AuthBearer, Value: "super-secret-token"}, Tools: []ToolCache{{Name: "x"}}},
	}}))
	m := NewManager(ManagerOptions{WorkspaceRoot: ws})

	out, err := listServers(m)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "a", out[0].Name)
	assert.Equal(t, redacted, out[0].Auth.Value, "bearer value must be masked")
	assert.Equal(t, AuthBearer, out[0].Auth.Type)
	assert.Equal(t, 1, out[0].Tools)

	// Belt-and-suspenders: the rendered JSON never carries the plaintext.
	rendered, err := json.Marshal(out)
	require.NoError(t, err)
	assert.NotContains(t, string(rendered), "super-secret-token")
}

func TestAddServer_ConnectsAndCachesTools(t *testing.T) {
	ft := &fakeTransport{tools: []mcpclient.Tool{
		{Name: "add", InputSchema: json.RawMessage(`{"type":"object","required":["a","b"]}`)},
	}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer m.Close()

	res, err := addServer(context.Background(), m, "calc", "http://x/mcp", "calculator", "http", Auth{Type: AuthBearer, Value: "tok"})
	require.NoError(t, err)
	assert.Equal(t, "added", res.Status)
	assert.Equal(t, 1, res.Tools)
	assert.Equal(t, redacted, res.Auth.Value)

	// Config persisted with the tools/list cache.
	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, cfg.Servers[0].Tools, 1)
	assert.Equal(t, "add", cfg.Servers[0].Tools[0].Name)

	// The connection was warmed and is reused by a follow-up call.
	c, err := m.Conn(context.Background(), "calc")
	require.NoError(t, err)
	ini, _, _ := ft.snapshot()
	assert.Equal(t, 1, ini, "add must not reconnect for the subsequent Conn")
	_, ok := c.cachedTool("add")
	assert.True(t, ok)
}

func TestAddServer_DuplicateNameRejected(t *testing.T) {
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add"}}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{})
	defer m.Close()
	require.NoError(t, WriteConfig(ws, &Config{Servers: []Server{{Name: "calc", URL: "http://x", Transport: "http"}}}))

	_, err := addServer(context.Background(), m, "calc", "http://y", "", "http", Auth{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestAddServer_RejectsNonHTTP(t *testing.T) {
	ft := &fakeTransport{}
	m, _ := newManagerWithFake(t, ft, ManagerOptions{})
	defer m.Close()
	_, err := addServer(context.Background(), m, "calc", "http://x", "", "stdio", Auth{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "http")
}

func TestRemoveServer_RemovesAndDropsConnection(t *testing.T) {
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add"}}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{})
	defer m.Close()
	_, err := addServer(context.Background(), m, "calc", "http://x", "", "http", Auth{Type: AuthBearer, Value: "tok"})
	require.NoError(t, err)
	_, _ = m.Conn(context.Background(), "calc") // warm the connection

	res, err := removeServer(m, "calc")
	require.NoError(t, err)
	assert.Equal(t, "removed", res.Status)
	assert.True(t, ft.closed, "cached connection must be dropped on remove")

	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	assert.Empty(t, cfg.Servers)
}

func TestCallTool_RejectsUnknownToolBeforeCall(t *testing.T) {
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add"}}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer m.Close()
	require.NoError(t, WriteConfig(ws, &Config{Servers: []Server{{
		Name: "calc", URL: "http://x", Transport: "http",
		Tools: []ToolCache{{Name: "add", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}}}))

	_, err := callTool(context.Background(), m, "calc", "nope", json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not advertised")
	_, _, calls := ft.snapshot()
	assert.Equal(t, 0, calls, "unknown tool must not reach the server")
}

func TestCallTool_RejectsBadArgsBeforeCall(t *testing.T) {
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add"}}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer m.Close()
	require.NoError(t, WriteConfig(ws, &Config{Servers: []Server{{
		Name: "calc", URL: "http://x", Transport: "http",
		Tools: []ToolCache{{Name: "add", InputSchema: json.RawMessage(`{"type":"object","required":["a"],"properties":{"a":{"type":"integer"}}}`)}},
	}}}))

	_, err := callTool(context.Background(), m, "calc", "add", json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required field")
	_, _, calls := ft.snapshot()
	assert.Equal(t, 0, calls, "bad args must not reach the server")
}

func TestCallTool_Success(t *testing.T) {
	ft := &fakeTransport{
		tools:      []mcpclient.Tool{{Name: "add", InputSchema: json.RawMessage(`{"type":"object","required":["a","b"]}`)}},
		callResult: &mcpclient.ToolsCallResult{Content: []mcpclient.Content{{Type: "text", Text: "42"}}},
	}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer m.Close()
	require.NoError(t, WriteConfig(ws, &Config{Servers: []Server{{
		Name: "calc", URL: "http://x", Transport: "http",
		Tools: []ToolCache{{Name: "add", InputSchema: json.RawMessage(`{"type":"object","required":["a","b"]}`)}},
	}}}))

	out, err := callTool(context.Background(), m, "calc", "add", json.RawMessage(`{"a":1,"b":2}`))
	require.NoError(t, err)
	require.Len(t, out.Content, 1)
	assert.Equal(t, "42", out.Content[0].Text)
}

func TestCallTool_RemoteError(t *testing.T) {
	ft := &fakeTransport{
		tools:      []mcpclient.Tool{{Name: "boom", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		callResult: &mcpclient.ToolsCallResult{IsError: true, Content: []mcpclient.Content{{Type: "text", Text: "kaboom"}}},
	}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer m.Close()
	require.NoError(t, WriteConfig(ws, &Config{Servers: []Server{{
		Name: "calc", URL: "http://x", Transport: "http",
		Tools: []ToolCache{{Name: "boom", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}}}))

	_, err := callTool(context.Background(), m, "calc", "boom", json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote tool error")
}

func TestCallTool_RefreshOnMiss(t *testing.T) {
	// Config cache is empty, but the live server advertises the tool. The
	// refresh-on-miss path re-lists and accepts the call.
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer m.Close()
	require.NoError(t, WriteConfig(ws, &Config{Servers: []Server{{Name: "calc", URL: "http://x", Transport: "http"}}}))

	out, err := callTool(context.Background(), m, "calc", "add", json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Len(t, out.Content, 1)

	// The refreshed cache is persisted.
	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, cfg.Servers[0].Tools, 1)
	assert.Equal(t, "add", cfg.Servers[0].Tools[0].Name)
}

// ---------------------------------------------------------------------------
// Log redaction (task 5.2) — auth never appears in structured logs.
// ---------------------------------------------------------------------------

func TestLog_NoPlaintextAuth(t *testing.T) {
	const secret = "do-not-log-me"

	core, recorded := observer.New(zapcore.DebugLevel)
	prev := logger.L()
	logger.SetDefault(zap.New(core))
	t.Cleanup(func() { logger.SetDefault(prev) })

	// Drive an add + a connect that logs at Debug, and trigger an error path
	// (unknown tool) which may log.
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	m, _ := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer m.Close()

	_, err := addServer(context.Background(), m, "calc", "http://x", "", "http", Auth{Type: AuthBearer, Value: secret})
	require.NoError(t, err)
	_, _ = callTool(context.Background(), m, "calc", "missing", json.RawMessage(`{}`))

	for _, entry := range recorded.All() {
		line := entry.Message
		for _, f := range entry.Context {
			line += " " + fieldString(f)
		}
		assert.NotContains(t, line, secret, "log line leaked auth: %s", line)
	}
}

// fieldString stringifies a zap field for the leak assertion.
func fieldString(f zapcore.Field) string {
	switch f.Type {
	case zapcore.StringType:
		return f.String
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Per-user isolation (task 8.2)
// ---------------------------------------------------------------------------

func TestPerUserIsolation(t *testing.T) {
	aliceWS := t.TempDir()
	bobWS := t.TempDir()
	require.NoError(t, WriteConfig(aliceWS, &Config{Servers: []Server{
		{Name: "alice-srv", URL: "http://alice/mcp", Transport: "http", Auth: Auth{Type: AuthBearer, Value: "alice-secret"}},
	}}))
	require.NoError(t, WriteConfig(bobWS, &Config{Servers: []Server{
		{Name: "bob-srv", URL: "http://bob/mcp", Transport: "http", Auth: Auth{Type: AuthBearer, Value: "bob-secret"}},
	}}))

	aliceMgr := NewManager(ManagerOptions{WorkspaceRoot: aliceWS, ConnectTimeout: time.Second, CallTimeout: time.Second})
	bobMgr := NewManager(ManagerOptions{WorkspaceRoot: bobWS, ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer aliceMgr.Close()
	defer bobMgr.Close()

	// Each manager reads only its own config.
	aliceServers, err := listServers(aliceMgr)
	require.NoError(t, err)
	require.Len(t, aliceServers, 1)
	assert.Equal(t, "alice-srv", aliceServers[0].Name)

	bobServers, err := listServers(bobMgr)
	require.NoError(t, err)
	require.Len(t, bobServers, 1)
	assert.Equal(t, "bob-srv", bobServers[0].Name)

	// Alice's manager cannot see Bob's server, and vice versa.
	_, err = aliceMgr.Conn(context.Background(), "bob-srv")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
	_, err = bobMgr.Conn(context.Background(), "alice-srv")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")

	// Configs on disk remain disjoint.
	a, _ := LoadConfig(aliceWS)
	b, _ := LoadConfig(bobWS)
	assert.Equal(t, "alice-secret", a.Servers[0].Auth.Value)
	assert.Equal(t, "bob-secret", b.Servers[0].Auth.Value)
	assert.NotContains(t, mustJSON(a), "bob-secret")
	assert.NotContains(t, mustJSON(b), "alice-secret")
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---------------------------------------------------------------------------
// Default transport factory builds auth-bearing headers (task 4.4)
// ---------------------------------------------------------------------------

func TestAuthHeaders(t *testing.T) {
	h := authHeaders(Auth{Type: AuthBearer, Value: "tok"})
	assert.Equal(t, "Bearer tok", h["Authorization"])

	h = authHeaders(Auth{Type: AuthAPIKey, Value: "k", Header: "X-Custom"})
	assert.Equal(t, "k", h["X-Custom"])

	h = authHeaders(Auth{Type: AuthAPIKey, Value: "k"})
	assert.Equal(t, "k", h[defaultAPIKeyHeader])

	h = authHeaders(Auth{Type: AuthBasic, Username: "u", Password: "p"})
	assert.Equal(t, "Basic dTpw", h["Authorization"])

	assert.Nil(t, authHeaders(Auth{Type: AuthNone}))
	assert.Nil(t, authHeaders(Auth{Type: AuthBearer, Value: ""}))
}

func TestIsMCPTool(t *testing.T) {
	assert.True(t, IsMCPTool(ToolCall))
	assert.True(t, IsMCPTool(ToolListServers))
	assert.False(t, IsMCPTool("xizhi_read_file"))
}

func TestAnyMCPTool(t *testing.T) {
	assert.False(t, AnyMCPTool([]string{"xizhi_read_file"}, []string{"luban_list_skills"}))
	assert.True(t, AnyMCPTool([]string{"xizhi_read_file"}, []string{ToolCall}))
}
