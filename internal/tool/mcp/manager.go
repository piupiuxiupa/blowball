package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/tool/mcpclient"
	"go.uber.org/zap"
)

// TransportFactory builds a Transport for a per-user server. It is a var so
// tests can inject a mock transport without reaching into the Manager; the
// default implementation builds an HTTPTransport with the server's injected
// auth headers.
type TransportFactory = func(server Server, tc TimeoutConfig) (mcpclient.Transport, error)

// TimeoutConfig bundles the per-server timeout knobs handed to a transport.
type TimeoutConfig struct {
	Connect time.Duration
	Call    time.Duration
}
// DefaultTransportFactory builds an HTTPTransport carrying the server's static
// auth headers (leak invariant #3: auth is injected here, server-side, and
// never appears in a tool result or log). The HTTP client timeout is set to
// the total call timeout as a per-request safety net; precise control is
// provided by the caller-bound contexts the Manager passes to each transport
// method.
var DefaultTransportFactory TransportFactory = func(server Server, tc TimeoutConfig) (mcpclient.Transport, error) {
	callTimeout := tc.Call
	if callTimeout <= 0 {
		callTimeout = defaultTotalCallTimeout
	}
	headers := authHeaders(server.Auth)
	return mcpclient.NewHTTPTransport(server.URL, headers, callTimeout), nil
}

// conn is one turn-scoped connection: the transport (with its session-id
// state) plus the tools/list snapshot captured at connect time. The snapshot
// is used to validate mcp_call args before the round trip.
type conn struct {
	transport mcpclient.Transport
	tools     []mcpclient.Tool
}

// Manager is the turn-scoped, per-user MCP connection manager. It is created
// once per turn (in the orchestrator factory) and closed at turn end. It
// lazily connects to a configured server on first use, reuses the connection
// for the rest of the turn, and is destroyed when the turn ends — there is no
// cross-turn state, so one user's authenticated connection can never be served
// to another user's turn (leak invariant: per-user credential isolation).
//
// Connections are keyed by server name within this manager, and the manager is
// itself scoped to a single user/turn, so the keying is inherently user-correct
// by construction (see design D1/D5).
type Manager struct {
	workspaceRoot    string
	connectTimeout   time.Duration
	callTimeout      time.Duration
	transportFactory TransportFactory

	mu     sync.Mutex
	conns  map[string]*conn // serverName -> connection
	closed bool
}

// ManagerOptions configures a Manager. Zero values fall back to the package
// defaults.
type ManagerOptions struct {
	WorkspaceRoot    string
	ConnectTimeout   time.Duration
	CallTimeout      time.Duration
	TransportFactory TransportFactory
}

// NewManager builds a turn-scoped manager bound to workspaceRoot (the caller's
// workspace). The workspace root determines which `.blowball/mcp/` server
// directory tree is read and to which user every connection authenticates.
func NewManager(opts ManagerOptions) *Manager {
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = defaultConnectTimeout
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = defaultTotalCallTimeout
	}
	tf := opts.TransportFactory
	if tf == nil {
		tf = DefaultTransportFactory
	}
	return &Manager{
		workspaceRoot:    opts.WorkspaceRoot,
		connectTimeout:   opts.ConnectTimeout,
		callTimeout:      opts.CallTimeout,
		transportFactory: tf,
		conns:            make(map[string]*conn),
	}
}

// WorkspaceRoot returns the workspace this manager is scoped to.
func (m *Manager) WorkspaceRoot() string { return m.workspaceRoot }

// Conn returns the turn-scoped connection for serverName, connecting lazily on
// first use. The connection is loaded from the per-user config (so it carries
// this user's auth) and cached for the rest of the turn. ctx bounds the
// connect handshake (a child context capped at the connect timeout).
func (m *Manager) Conn(ctx context.Context, serverName string) (*conn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("mcp manager closed")
	}
	if c, ok := m.conns[serverName]; ok {
		return c, nil
	}

	cfg, err := LoadConfig(m.workspaceRoot)
	if err != nil {
		return nil, err
	}
	server, ok := cfg.Server(serverName)
	if !ok {
		return nil, fmt.Errorf("mcp server %q is not configured", serverName)
	}

	c, err := m.connect(ctx, server)
	if err != nil {
		return nil, err
	}
	m.conns[serverName] = c
	return c, nil
}

// connect establishes a fresh connection to server: build the transport with
// injected auth, run the MCP handshake, and capture the tools/list snapshot.
// ctx bounds the handshake via a connect-timeout child; the handshake-hang
// scenario (task 2.2) is caught by that child context rather than the total
// call budget.
func (m *Manager) connect(ctx context.Context, server Server) (*conn, error) {
	transport, err := m.transportFactory(server, TimeoutConfig{Connect: m.connectTimeout, Call: m.callTimeout})
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: build transport: %w", server.Name, err)
	}

	initCtx, cancel := context.WithTimeout(ctx, m.connectTimeout)
	defer cancel()
	if _, err := transport.Initialize(initCtx, mcpclient.InitializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities:    map[string]any{},
		ClientInfo:      mcpclient.ClientInfo{Name: "blowball", Version: "0.1.0"},
	}); err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("mcp server %q: initialize: %w", server.Name, err)
	}

	listCtx, cancel := context.WithTimeout(ctx, m.connectTimeout)
	defer cancel()
	list, err := transport.ListTools(listCtx)
	if err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("mcp server %q: list tools: %w", server.Name, err)
	}
	logger.L().Debug("mcp connected",
		zap.String("server", server.Name),
		zap.Int("tools", len(list.Tools)))
	return &conn{transport: transport, tools: list.Tools}, nil
}

// Call invokes the named tool on the cached connection for serverName. ctx
// bounds the remote call (the Manager wraps mcp_call's whole operation in the
// total-call timeout before calling here, so the remaining budget applies). A
// remote isError result is surfaced to the caller as an error so the agent
// loop emits a tool_error event.
func (c *conn) Call(ctx context.Context, name string, args []byte) (*mcpclient.ToolsCallResult, error) {
	result, err := c.transport.CallTool(ctx, mcpclient.ToolsCallParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err
	}
	if result.IsError {
		return result, remoteToolError(result)
	}
	return result, nil
}

// cachedTool returns the cached tools/list entry for name (ok=false if the tool
// is not advertised by this server). Used by mcp_call to validate the tool name
// and args before the round trip.
func (c *conn) cachedTool(name string) (mcpclient.Tool, bool) {
	for _, t := range c.tools {
		if t.Name == name {
			return t, true
		}
	}
	return mcpclient.Tool{}, false
}

// tools returns a copy of the cached tool list.
func (c *conn) toolsList() []mcpclient.Tool {
	out := make([]mcpclient.Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

// ConnectServer establishes (and caches) a connection to the given server and
// returns its tools/list snapshot. mcp_add_server uses it to validate the new
// entry and capture the schema cache in one connect, so the subsequent
// mcp_call reuses the already-warm connection. If a connection for the name is
// already cached it is reused.
func (m *Manager) ConnectServer(ctx context.Context, server Server) ([]mcpclient.Tool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("mcp manager closed")
	}
	if c, ok := m.conns[server.Name]; ok {
		return c.toolsList(), nil
	}
	c, err := m.connect(ctx, server)
	if err != nil {
		return nil, err
	}
	m.conns[server.Name] = c
	return c.toolsList(), nil
}

// ListServerTools re-runs tools/list on the cached connection for serverName
// (connecting if needed) and returns the fresh snapshot. mcp_call uses it to
// refresh a stale schema cache (task 4.3) when a tool is absent from the
// persisted cache.
func (m *Manager) ListServerTools(ctx context.Context, serverName string) ([]mcpclient.Tool, error) {
	c, err := m.Conn(ctx, serverName)
	if err != nil {
		return nil, err
	}
	listCtx, cancel := context.WithTimeout(ctx, m.connectTimeout)
	defer cancel()
	list, err := c.transport.ListTools(listCtx)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: refresh tools/list: %w", serverName, err)
	}
	// Refresh the in-memory snapshot so subsequent calls see the fresh list.
	m.mu.Lock()
	if cur, ok := m.conns[serverName]; ok {
		cur.tools = list.Tools
	}
	m.mu.Unlock()
	return list.Tools, nil
}

// DropConnection closes and forgets the cached connection for serverName, if
// any. mcp_remove_server calls it so a removed server's authenticated
// connection cannot outlive its removal within the turn.
func (m *Manager) DropConnection(serverName string) {
	m.mu.Lock()
	c, ok := m.conns[serverName]
	if ok {
		delete(m.conns, serverName)
	}
	m.mu.Unlock()
	if ok {
		_ = c.transport.Close()
	}
}

// Close releases every cached connection. Safe to call once at turn end;
// subsequent Conn/Call attempts error. It is idempotent.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	var first error
	for name, c := range m.conns {
		if err := c.transport.Close(); err != nil && first == nil {
			first = fmt.Errorf("mcp server %q: close: %w", name, err)
		}
	}
	m.conns = nil
	return first
}

// remoteToolError formats a remote isError result as an error. Auth/secret
// values never appear here — the message is assembled from the remote Content
// text only.
func remoteToolError(result *mcpclient.ToolsCallResult) error {
	var parts []string
	for _, c := range result.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	if len(parts) == 0 {
		return fmt.Errorf("remote tool returned an error")
	}
	return fmt.Errorf("remote tool error: %s", joinStrings(parts, "; "))
}

func joinStrings(parts []string, sep string) string {
	return strings.Join(parts, sep)
}
