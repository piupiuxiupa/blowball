package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	initErr    error
	listErr    error
	initDelay  time.Duration
	listDelay  time.Duration
	callDelay  time.Duration
	closed     bool
}

func (f *fakeTransport) Initialize(ctx context.Context, _ mcpclient.InitializeParams) (*mcpclient.InitializeResult, error) {
	f.mu.Lock()
	f.initCount++
	delay := f.initDelay
	initErr := f.initErr
	f.mu.Unlock()
	if initErr != nil {
		return nil, initErr
	}
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
	listErr := f.listErr
	f.mu.Unlock()
	if listErr != nil {
		return nil, listErr
	}
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
	opts.TransportFactory = func(server Server, _ TimeoutConfig) (mcpclient.Transport, error) {
		return ft, nil
	}
	return NewManager(opts), ws
}

// writeServers stands up one or more servers on disk by writing each to its own
// per-server config file (mirroring WriteServer / mcp_add_server output),
// without any network activity.
func writeServers(t *testing.T, ws string, servers ...Server) {
	t.Helper()
	for _, s := range servers {
		require.NoError(t, WriteServer(ws, s))
	}
}

// writeServerRaw writes raw bytes as {name}/config.json (creating the
// directory), bypassing validation. Used to set up malformed / hand-crafted
// fixtures.
func writeServerRaw(t *testing.T, ws, name, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(serversDir(ws), name), 0o755))
	require.NoError(t, os.WriteFile(serverConfigPath(ws, name), []byte(body), 0o644))
}

// ---------------------------------------------------------------------------
// Name validation (tasks 1.1, 5.2, 4.2)
// ---------------------------------------------------------------------------

func TestValidateName(t *testing.T) {
	for _, n := range []string{"github", "my-mcp", "svc_2", "a", "A1", strings.Repeat("a", 64)} {
		assert.NoError(t, ValidateName(n), "expected %q to be valid", n)
	}
	// Each of these is rejected: path separators, leading dot, dots, spaces,
	// other punctuation, and oversize.
	for _, n := range []string{
		"",        // empty
		".h",      // leading dot
		"a b",     // whitespace
		"a.b",     // dot
		"a/b",     // path separator
		"../x",    // traversal
		`a\b`,     // backslash separator
		"a:b",     // other punctuation
		"-lead",   // leading hyphen (not alphanumeric)
		"_lead",   // leading underscore (not alphanumeric)
		strings.Repeat("a", 65), // oversize
	} {
		assert.Error(t, ValidateName(n), "expected %q to be rejected", n)
	}
}

func TestValidateName_ErrorMentionsRulesAndExamples(t *testing.T) {
	// task 4.2: an invalid name yields a message with the rule and
	// positive/negative examples so the agent can self-correct.
	err := ValidateName("a.b")
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "a.b", "error should name the offending value")
	assert.Contains(t, msg, "github", "error should show a positive example")
	assert.Contains(t, msg, "^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$", "error should state the rule")
}

// ---------------------------------------------------------------------------
// Config: load / validate / write / add / remove (tasks 1.2-1.4, 2.1-2.5)
// ---------------------------------------------------------------------------

func TestLoadConfig_MissingDirIsEmpty(t *testing.T) {
	cfg, err := LoadConfig(t.TempDir())
	require.NoError(t, err)
	require.Empty(t, cfg.Servers)
}

func TestLoadConfig_EmptyServersDirIsEmpty(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(serversDir(ws), 0o755))
	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Empty(t, cfg.Servers)
}

func TestLoadConfig_Valid(t *testing.T) {
	ws := t.TempDir()
	writeServers(t, ws, Server{
		Name: "calc", URL: "http://x/mcp", Transport: "http",
		Auth: Auth{Type: AuthBearer, Value: "tok"},
	})
	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, cfg.Servers, 1)
	assert.Equal(t, "calc", cfg.Servers[0].Name)
	assert.Equal(t, AuthBearer, cfg.Servers[0].Auth.Type)
	assert.Equal(t, "tok", cfg.Servers[0].Auth.Value)
}

func TestLoadConfig_NameTakenFromDirectoryNotFileBody(t *testing.T) {
	// task 1.3 / spec: the directory name is authoritative; a stray `name`
	// field inside the file body is ignored.
	ws := t.TempDir()
	writeServerRaw(t, ws, "github", `{"name":"should-be-ignored","url":"http://x","transport":"http"}`)
	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, cfg.Servers, 1)
	assert.Equal(t, "github", cfg.Servers[0].Name, "name must come from the directory")
}

func TestLoadConfig_MalformedServerSkipped(t *testing.T) {
	// task 5.1 / spec: a single corrupted server is unavailable but does not
	// crash or break the other servers.
	ws := t.TempDir()
	writeServers(t, ws, Server{Name: "good", URL: "http://x", Transport: "http"})
	writeServerRaw(t, ws, "bad", "{not json")
	cfg, err := LoadConfig(ws)
	require.NoError(t, err, "a malformed server must not fail the whole load")
	require.Len(t, cfg.Servers, 1)
	assert.Equal(t, "good", cfg.Servers[0].Name)
	_, ok := cfg.Server("bad")
	assert.False(t, ok, "malformed server must be unavailable")
}

func TestLoadConfig_InvalidFieldsServerSkipped(t *testing.T) {
	// A directory that parses but fails field validation (e.g. missing url,
	// bad transport) is skipped too — the server is unavailable, not a crash.
	ws := t.TempDir()
	writeServers(t, ws, Server{Name: "good", URL: "http://x", Transport: "http"})
	writeServerRaw(t, ws, "nourl", `{"transport":"http"}`)
	writeServerRaw(t, ws, "stdio", `{"url":"http://x","transport":"stdio"}`)
	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, cfg.Servers, 1)
	assert.Equal(t, "good", cfg.Servers[0].Name)
}

func TestLoadConfig_MissingConfigJSONSkipped(t *testing.T) {
	// A server directory with no config.json is not a usable server.
	ws := t.TempDir()
	writeServers(t, ws, Server{Name: "good", URL: "http://x", Transport: "http"})
	require.NoError(t, os.MkdirAll(filepath.Join(serversDir(ws), "empty"), 0o755))
	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, cfg.Servers, 1)
	assert.Equal(t, "good", cfg.Servers[0].Name)
}

func TestLoadConfig_SkipsNonServerEntries(t *testing.T) {
	// task 5.4: enumeration skips non-directories, hidden directories, and
	// directories whose name fails ValidateName.
	ws := t.TempDir()
	writeServers(t, ws, Server{Name: "good", URL: "http://x", Transport: "http"})
	// stray top-level file (not a directory).
	require.NoError(t, os.WriteFile(filepath.Join(serversDir(ws), "stray.txt"), []byte("x"), 0o644))
	// hidden directory.
	require.NoError(t, os.MkdirAll(filepath.Join(serversDir(ws), ".hidden"), 0o755))
	// invalid-name directory (contains a dot).
	require.NoError(t, os.MkdirAll(filepath.Join(serversDir(ws), "a.b"), 0o755))
	// leftover atomic-write temp file inside a server directory.
	require.NoError(t, os.WriteFile(filepath.Join(serversDir(ws), "good", ".mcp-config-tmp"), []byte("x"), 0o644))

	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, cfg.Servers, 1)
	assert.Equal(t, "good", cfg.Servers[0].Name)
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

func TestValidate_RejectsInvalidName(t *testing.T) {
	c := &Config{Servers: []Server{{Name: "a.b", URL: "http://x", Transport: "http"}}}
	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid")
}

func TestValidate_RequiresNameAndURL(t *testing.T) {
	require.Error(t, (&Config{Servers: []Server{{URL: "http://x", Transport: "http"}}}).Validate())
	require.Error(t, (&Config{Servers: []Server{{Name: "s", Transport: "http"}}}).Validate())
}

func TestWriteServer_AtomicAndRoundTrips(t *testing.T) {
	ws := t.TempDir()
	s := Server{
		Name: "calc", URL: "http://x/mcp", Transport: "http",
		Auth:  Auth{Type: AuthBearer, Value: "tok"},
		Tools: []ToolCache{{Name: "add", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	require.NoError(t, WriteServer(ws, s))

	// The server lives in its own directory with exactly one file (no leftover
	// temp from the atomic write).
	dir := filepath.Join(serversDir(ws), "calc")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, ConfigFile, entries[0].Name())

	// The file body must NOT carry a server-level name — it is
	// directory-resident. (Tool entries legitimately have their own "name".)
	body, err := os.ReadFile(serverConfigPath(ws, "calc"))
	require.NoError(t, err)
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &top))
	_, hasServerName := top["name"]
	assert.False(t, hasServerName, "server name must not be persisted in the file body")

	loaded, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, loaded.Servers, 1)
	assert.Equal(t, "calc", loaded.Servers[0].Name, "name back-filled from directory")
	assert.Equal(t, "tok", loaded.Servers[0].Auth.Value)
	require.Len(t, loaded.Servers[0].Tools, 1)
	assert.Equal(t, "add", loaded.Servers[0].Tools[0].Name)
}

func TestWriteServer_RejectsInvalidName(t *testing.T) {
	ws := t.TempDir()
	err := WriteServer(ws, Server{Name: "a/b", URL: "http://x", Transport: "http"})
	require.Error(t, err)
	// Nothing is written for an invalid name.
	_, statErr := os.Stat(serversDir(ws))
	assert.True(t, os.IsNotExist(statErr))
}

func TestWriteServer_RejectsBadFields(t *testing.T) {
	ws := t.TempDir()
	err := WriteServer(ws, Server{Name: "s", Transport: "stdio"})
	require.Error(t, err)
}

func TestAddServer_CreatesDirectoryAndFile(t *testing.T) {
	// task 5.3: AddServer creates the directory and writes config.json.
	ws := t.TempDir()
	require.NoError(t, AddServer(ws, Server{Name: "github", URL: "http://x", Transport: "http"}))
	_, err := os.Stat(filepath.Join(serversDir(ws), "github", ConfigFile))
	require.NoError(t, err, "config.json must exist after AddServer")
}

func TestAddServer_DuplicateRejected(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, AddServer(ws, Server{Name: "s", URL: "http://x", Transport: "http"}))
	err := AddServer(ws, Server{Name: "s", URL: "http://y", Transport: "http"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	// The original entry is untouched.
	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, cfg.Servers, 1)
	assert.Equal(t, "http://x", cfg.Servers[0].URL)
}

func TestAddServer_RejectsInvalidName(t *testing.T) {
	ws := t.TempDir()
	err := AddServer(ws, Server{Name: "a.b", URL: "http://x", Transport: "http"})
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(serversDir(ws), "a.b"))
	assert.True(t, os.IsNotExist(statErr))
}

func TestRemoveServer_RemovesDirectoryAndPreservesOthers(t *testing.T) {
	// task 5.3: RemoveServer deletes the directory and leaves other servers.
	ws := t.TempDir()
	writeServers(t, ws,
		Server{Name: "a", URL: "http://a", Transport: "http"},
		Server{Name: "b", URL: "http://b", Transport: "http"})

	ok, err := RemoveServer(ws, "a")
	require.NoError(t, err)
	assert.True(t, ok)

	// Directory gone.
	_, statErr := os.Stat(filepath.Join(serversDir(ws), "a"))
	assert.True(t, os.IsNotExist(statErr))

	// The other server survives.
	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, cfg.Servers, 1)
	assert.Equal(t, "b", cfg.Servers[0].Name)

	// Removing a missing server is a clean no-op (ok=false, no error).
	ok, err = RemoveServer(ws, "missing")
	require.NoError(t, err)
	assert.False(t, ok)

	// An invalid name is treated as not-present rather than an error.
	ok, err = RemoveServer(ws, "a/b")
	require.NoError(t, err)
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// Turn-scoped connection reuse + timeouts (tasks 2.1, 2.2)
// ---------------------------------------------------------------------------

func TestManager_ConnectionReuseWithinTurn(t *testing.T) {
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add"}}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer m.Close()
	writeServers(t, ws, Server{Name: "calc", URL: "http://x", Transport: "http"})

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
	writeServers(t, ws, Server{Name: "calc", URL: "http://x", Transport: "http"})

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
	writeServers(t, ws, Server{
		Name: "calc", URL: "http://x", Transport: "http",
		Tools: []ToolCache{{Name: "add", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})

	// callTool wraps the whole op in the total-call timeout; a slow call must
	// surface a deadline error.
	_, err := callTool(context.Background(), m, "calc", "add", json.RawMessage(`{}`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "want DeadlineExceeded, got %v", err)
}

func TestManager_CloseDropsConnections(t *testing.T) {
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add"}}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{})
	writeServers(t, ws, Server{Name: "calc", URL: "http://x", Transport: "http"})
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
	writeServers(t, ws, Server{
		Name: "a", URL: "http://a", Transport: "http",
		Auth:  Auth{Type: AuthBearer, Value: "super-secret-token"},
		Tools: []ToolCache{{Name: "x"}},
	})
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

	// Config persisted as a per-server file with the tools/list cache.
	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, cfg.Servers, 1)
	assert.Equal(t, "calc", cfg.Servers[0].Name)
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

func TestAddServer_InvalidNameRejected(t *testing.T) {
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add"}}}
	m, _ := newManagerWithFake(t, ft, ManagerOptions{})
	defer m.Close()

	_, err := addServer(context.Background(), m, "a.b", "http://x", "", "http", Auth{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid")
	assert.Contains(t, err.Error(), "github") // rule/example guidance present
	_, _, calls := ft.snapshot()
	assert.Equal(t, 0, calls, "an invalid name must not contact the server")
}

func TestAddServer_DuplicateNameRejected(t *testing.T) {
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add"}}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{})
	defer m.Close()
	writeServers(t, ws, Server{Name: "calc", URL: "http://x", Transport: "http"})

	_, err := addServer(context.Background(), m, "calc", "http://y", "", "http", Auth{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	ini, _, _ := ft.snapshot()
	assert.Equal(t, 0, ini, "a duplicate must be rejected before connecting")
}

func TestAddServer_RejectsNonHTTP(t *testing.T) {
	ft := &fakeTransport{}
	m, _ := newManagerWithFake(t, ft, ManagerOptions{})
	defer m.Close()
	_, err := addServer(context.Background(), m, "calc", "http://x", "", "stdio", Auth{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "http")
	ini, _, _ := ft.snapshot()
	assert.Equal(t, 0, ini, "a bad transport must be rejected before connecting")
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
	_, statErr := os.Stat(filepath.Join(serversDir(ws), "calc"))
	assert.True(t, os.IsNotExist(statErr), "server directory must be removed")
}

func TestRemoveServer_NotConfigured(t *testing.T) {
	m := NewManager(ManagerOptions{WorkspaceRoot: t.TempDir()})
	defer m.Close()
	_, err := removeServer(m, "missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

func TestCallTool_RejectsUnknownToolBeforeCall(t *testing.T) {
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add"}}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer m.Close()
	writeServers(t, ws, Server{
		Name: "calc", URL: "http://x", Transport: "http",
		Tools: []ToolCache{{Name: "add", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})

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
	writeServers(t, ws, Server{
		Name: "calc", URL: "http://x", Transport: "http",
		Tools: []ToolCache{{Name: "add", InputSchema: json.RawMessage(`{"type":"object","required":["a"],"properties":{"a":{"type":"integer"}}}`)}},
	})

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
	writeServers(t, ws, Server{
		Name: "calc", URL: "http://x", Transport: "http",
		Tools: []ToolCache{{Name: "add", InputSchema: json.RawMessage(`{"type":"object","required":["a","b"]}`)}},
	})

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
	writeServers(t, ws, Server{
		Name: "calc", URL: "http://x", Transport: "http",
		Tools: []ToolCache{{Name: "boom", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})

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
	writeServers(t, ws, Server{Name: "calc", URL: "http://x", Transport: "http"})

	out, err := callTool(context.Background(), m, "calc", "add", json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Len(t, out.Content, 1)

	// The refreshed cache is persisted to the server's own file.
	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	require.Len(t, cfg.Servers, 1)
	require.Len(t, cfg.Servers[0].Tools, 1)
	assert.Equal(t, "add", cfg.Servers[0].Tools[0].Name)
}

// ---------------------------------------------------------------------------
// Log redaction — auth never appears in structured logs.
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
// Per-user isolation
// ---------------------------------------------------------------------------

func TestPerUserIsolation(t *testing.T) {
	aliceWS := t.TempDir()
	bobWS := t.TempDir()
	writeServers(t, aliceWS, Server{
		Name: "alice-srv", URL: "http://alice/mcp", Transport: "http",
		Auth: Auth{Type: AuthBearer, Value: "alice-secret"},
	})
	writeServers(t, bobWS, Server{
		Name: "bob-srv", URL: "http://bob/mcp", Transport: "http",
		Auth: Auth{Type: AuthBearer, Value: "bob-secret"},
	})

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
// Default transport factory builds auth-bearing headers
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
	assert.True(t, IsMCPTool(ToolListTools))
	assert.False(t, IsMCPTool("xizhi_read_file"))
}

func TestAnyMCPTool(t *testing.T) {
	assert.False(t, AnyMCPTool([]string{"xizhi_read_file"}, []string{"luban_list_skills"}))
	assert.True(t, AnyMCPTool([]string{"xizhi_read_file"}, []string{ToolCall}))
	assert.True(t, AnyMCPTool([]string{"xizhi_read_file"}, []string{ToolListTools}))
}

// ---------------------------------------------------------------------------
// mcp_list_tools: live discovery + async cache write-back (tasks 6.1, 6.2)
// ---------------------------------------------------------------------------

func TestListTools_ReturnsLiveToolList(t *testing.T) {
	// task 6.1: mcp_list_tools connects live and returns the server's tools
	// with name/description/input_schema.
	ft := &fakeTransport{tools: []mcpclient.Tool{
		{Name: "add", Description: "adds", InputSchema: json.RawMessage(`{"type":"object","required":["a","b"]}`)},
		{Name: "mul", Description: "multiplies", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer m.Close()
	writeServers(t, ws, Server{Name: "calc", URL: "http://x", Transport: "http"})

	out, err := listTools(context.Background(), m, "calc")
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, "add", out[0].Name)
	assert.Equal(t, "adds", out[0].Description)
	assert.JSONEq(t, `{"type":"object","required":["a","b"]}`, string(out[0].InputSchema))
	assert.Equal(t, "mul", out[1].Name)
}

func TestListTools_UnknownServerRejected(t *testing.T) {
	// task 6.1: an unconfigured server is rejected before any connection; no
	// write-back occurs.
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add"}}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{})
	defer m.Close()
	writeServers(t, ws, Server{Name: "calc", URL: "http://x", Transport: "http"})

	_, err := listTools(context.Background(), m, "ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
	ini, list, calls := ft.snapshot()
	assert.Equal(t, 0, ini+list+calls, "unknown server must not contact the server")
}

func TestListTools_ListFailureErrorsWithoutWriteBack(t *testing.T) {
	// task 6.1: a tools/list failure surfaces a clear error and triggers no
	// cache write-back.
	ft := &fakeTransport{listErr: errors.New("boom")}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer m.Close()
	writeServers(t, ws, Server{Name: "calc", URL: "http://x", Transport: "http"})

	_, err := listTools(context.Background(), m, "calc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mcp_list_tools")

	// The server's cache stays empty (no write-back on failure).
	require.Eventually(t, func() bool {
		cfg, _ := LoadConfig(ws)
		s, ok := cfg.Server("calc")
		return ok && len(s.Tools) == 0
	}, time.Second, 5*time.Millisecond)
}

func TestListTools_WriteBackUpdatesCacheAndDoesNotBlock(t *testing.T) {
	// task 6.2: the live result is returned immediately and the cache is
	// written back asynchronously (the return is not blocked by the write).
	ft := &fakeTransport{tools: []mcpclient.Tool{
		{Name: "add", Description: "adds", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer m.Close()
	writeServers(t, ws, Server{Name: "calc", URL: "http://x", Transport: "http"})

	out, err := listTools(context.Background(), m, "calc")
	require.NoError(t, err)
	require.Len(t, out, 1, "result returned before the write-back completes")

	// The async write-back eventually persists the discovered tools.
	require.Eventually(t, func() bool {
		cfg, _ := LoadConfig(ws)
		s, ok := cfg.Server("calc")
		return ok && len(s.Tools) == 1 && s.Tools[0].Name == "add"
	}, time.Second, 5*time.Millisecond)
}

func TestListTools_WriteBackSurvivesTurnCancellation(t *testing.T) {
	// task 6.2: cancelling the turn ctx after the result returned does not
	// abort the write-back, which runs on an independent background context.
	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "add"}}}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second})
	defer m.Close()
	writeServers(t, ws, Server{Name: "calc", URL: "http://x", Transport: "http"})

	ctx, cancel := context.WithCancel(context.Background())
	out, err := listTools(ctx, m, "calc")
	require.NoError(t, err)
	require.Len(t, out, 1)
	cancel() // cancel the turn ctx once the result is in hand

	require.Eventually(t, func() bool {
		cfg, _ := LoadConfig(ws)
		s, ok := cfg.Server("calc")
		return ok && len(s.Tools) == 1 && s.Tools[0].Name == "add"
	}, time.Second, 5*time.Millisecond)
}

func TestWriteBackToolsAsync_FailureOnlyLogs(t *testing.T) {
	// task 6.2: a write-back failure is logged as a warning and never panics
	// or affects the caller.
	core, recorded := observer.New(zapcore.WarnLevel)
	prev := logger.L()
	logger.SetDefault(zap.New(core))
	t.Cleanup(func() { logger.SetDefault(prev) })

	ws := t.TempDir()
	// "ghost" is not configured → persist returns an error inside the goroutine.
	writeBackToolsAsync(ws, "ghost", []mcpclient.Tool{{Name: "add"}})

	require.Eventually(t, func() bool {
		return recorded.FilterMessage("mcp_list_tools: async cache write-back failed (non-fatal)").Len() > 0
	}, time.Second, 5*time.Millisecond, "write-back failure must be logged as a warning")

	// Nothing was written.
	cfg, err := LoadConfig(ws)
	require.NoError(t, err)
	assert.Empty(t, cfg.Servers)
}
