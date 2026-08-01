package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/mcpclient"
	"github.com/lush/blowball/internal/tool/skill"
	"go.uber.org/zap"
)

// Registered tool names. These are the strings agents reference in their
// config `tools:` lists.
const (
	ToolListServers  = "mcp_list_servers"
	ToolAddServer    = "mcp_add_server"
	ToolRemoveServer = "mcp_remove_server"
	ToolCall         = "mcp_call"
	ToolListTools    = "mcp_list_tools"
)

// IsMCPTool reports whether name is one of the per-user mcp_* tools.
func IsMCPTool(name string) bool {
	switch name {
	case ToolListServers, ToolAddServer, ToolRemoveServer, ToolCall, ToolListTools:
		return true
	}
	return false
}

// mcpTools is the static list of per-user mcp_* tool names, used by the
// orchestrator wiring to decide whether an agent uses the family.
var mcpTools = []string{ToolListServers, ToolAddServer, ToolRemoveServer, ToolCall, ToolListTools}

// AnyMCPTool reports whether any agent in agents lists an mcp_* tool. It mirrors
// the operator-side needsLubanTools pattern so serve.go wires the family only
// when at least one agent references it.
func AnyMCPTool(toolsByAgent ...[]string) bool {
	want := make(map[string]struct{}, len(mcpTools))
	for _, n := range mcpTools {
		want[n] = struct{}{}
	}
	for _, tools := range toolsByAgent {
		for _, n := range tools {
			if _, ok := want[n]; ok {
				return true
			}
		}
	}
	return false
}

// Tools is the per-turn bundle backing the mcp_* tools. It wraps the
// turn-scoped Manager, which is bound to one user's workspace for one turn.
// Because the manager is per-turn and per-user, every tool invocation in the
// turn resolves the SAME user's config and reuses that user's connections —
// there is no cross-user state (leak invariant: per-user credential isolation
// holds by construction).
type Tools struct {
	manager *Manager
}

// NewTools builds a Tools bundle for a turn-scoped manager. The manager must be
// bound to the requesting user's workspace root.
func NewTools(manager *Manager) *Tools {
	return &Tools{manager: manager}
}

// Manager returns the underlying turn-scoped manager. Exposed so the
// orchestrator can close it at turn end.
func (t *Tools) Manager() *Manager { return t.manager }

// RegisterAll registers the five mcp_* tools into r.
func RegisterAll(r *tool.Registry, tools *Tools) error {
	if err := registerListServers(r, tools); err != nil {
		return err
	}
	if err := registerAddServer(r, tools); err != nil {
		return err
	}
	if err := registerRemoveServer(r, tools); err != nil {
		return err
	}
	if err := registerCall(r, tools); err != nil {
		return err
	}
	if err := registerListTools(r, tools); err != nil {
		return err
	}
	return nil
}

// registerListServers: mcp_list_servers — list the caller's configured servers
// with credentials redacted (leak invariant #1).
func registerListServers(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name: ToolListServers,
		Description: "List the per-user MCP servers configured in YOUR workspace " +
			"(`.blowball/mcp/{name}/config.json`, one directory per server). Returns name, " +
			"url, transport, description, the auth KIND (credentials are redacted), and how " +
			"many tools each server advertises. Use this to discover which servers you can " +
			"call with `mcp_call`. Credentials are never shown.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {},
			"additionalProperties": false
		}`),
		Execute: func(ctx context.Context, _ json.RawMessage) (any, error) {
			// userID is propagated for logging/identity; the manager is already
			// bound to the caller's workspace.
			_ = skill.UserIDFromContext(ctx)
			return listServers(tools.manager)
		},
	}
	return r.Register(spec)
}

// registerAddServer: mcp_add_server — validate, connect, cache tools/list, write.
func registerAddServer(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name: ToolAddServer,
		Description: "Add a per-user MCP server to YOUR workspace config " +
			"(`.blowball/mcp/config.json`). Only the remote HTTP (Streamable HTTP) " +
			"transport and static credentials (bearer / api-key / basic) are supported; " +
			"stdio and OAuth are not. The tool connects to the server, validates it is " +
			"reachable, and caches its `tools/list` so `mcp_call` can validate args " +
			"later. A duplicate name is rejected. Credentials are stored in your config " +
			"file and are NEVER returned by this tool.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {
					"type": "string",
					"description": "A short, unique server identifier used as the directory name. Must match ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$ (start with a letter or digit; only letters, digits, '_' and '-'; 1-64 chars). Examples: \"github\", \"my-mcp\", \"svc_2\". Spaces, dots, and slashes are rejected."
				},
				"url": {
					"type": "string",
					"description": "The Streamable HTTP endpoint URL of the MCP server."
				},
				"description": {
					"type": "string",
					"description": "Optional human-readable description of what this server provides."
				},
				"transport": {
					"type": "string",
					"description": "Transport type. Only \"http\" is accepted (default if omitted).",
					"default": "http"
				},
				"auth": {
					"type": "object",
					"description": "Static credentials, injected server-side on each call. One of: {\"type\":\"bearer\",\"value\":\"<token>\"}, {\"type\":\"api-key\",\"value\":\"<key>\",\"header\":\"X-API-Key\"}, or {\"type\":\"basic\",\"username\":\"...\",\"password\":\"...\"}. OAuth is not supported.",
					"properties": {
						"type": {"type": "string", "enum": ["bearer", "api-key", "basic"]},
						"value": {"type": "string"},
						"username": {"type": "string"},
						"password": {"type": "string"},
						"header": {"type": "string"}
					},
					"required": ["type"]
				}
			},
			"required": ["name", "url"],
			"additionalProperties": false
		}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Name        string `json:"name"`
				URL         string `json:"url"`
				Description string `json:"description"`
				Transport   string `json:"transport"`
				Auth        Auth    `json:"auth"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("mcp_add_server: parse args: %w", err)
			}
			return addServer(ctx, tools.manager, a.Name, a.URL, a.Description, a.Transport, a.Auth)
		},
	}
	return r.Register(spec)
}

// registerRemoveServer: mcp_remove_server — remove by name, preserve the rest.
func registerRemoveServer(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name: ToolRemoveServer,
		Description: "Remove a per-user MCP server from YOUR workspace config by name. " +
			"The other configured servers are preserved. Returns a confirmation that " +
			"never echoes any credential.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {
					"type": "string",
					"description": "The name of the server to remove."
				}
			},
			"required": ["name"],
			"additionalProperties": false
		}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("mcp_remove_server: parse args: %w", err)
			}
			return removeServer(tools.manager, a.Name)
		},
	}
	return r.Register(spec)
}

// registerCall: mcp_call — the meta-tool that invokes a remote tool, with
// server-side auth injection and pre-call validation (leak invariant #3).
func registerCall(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name: ToolCall,
		Description: "Call a tool on one of YOUR configured per-user MCP servers. " +
			"You pass the server name, the tool name, and the tool's arguments object. " +
			"The tool name and arguments are validated against the server's cached " +
			"`tools/list` BEFORE the call; an unknown tool or schema-violating args are " +
			"rejected without contacting the server. Authentication is injected " +
			"server-side and is never part of the input or output. If you are unsure " +
			"which tools a server offers, call `mcp_list_servers` first (tool counts are " +
			"shown); to discover exact tool names/args you may need to consult the " +
			"server's own documentation. A single call is bounded by the total-call " +
			"timeout (default 10s).",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"server": {
					"type": "string",
					"description": "The configured server name (see mcp_list_servers)."
				},
				"tool": {
					"type": "string",
					"description": "The remote tool name to invoke."
				},
				"args": {
					"type": "object",
					"description": "The arguments object for the remote tool.",
					"additionalProperties": true
				}
			},
			"required": ["server", "tool"],
			"additionalProperties": false
		}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Server string          `json:"server"`
				Tool   string          `json:"tool"`
				Args   json.RawMessage `json:"args"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("mcp_call: parse args: %w", err)
			}
			if len(a.Args) == 0 {
				a.Args = json.RawMessage(`{}`)
			}
			return callTool(ctx, tools.manager, a.Server, a.Tool, a.Args)
		},
	}
	return r.Register(spec)
}

// registerListTools: mcp_list_tools — discover one per-user server's tool
// catalogue live, returning name/description/input_schema for each tool. This
// is the authoritative entry point for discovering a per-user server's tool
// names and argument schemas. On success the result is written back to the
// server's config cache asynchronously (fire-and-forget) so subsequent
// mcp_call validations pass without a live refresh.
func registerListTools(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name: ToolListTools,
		Description: "Discover the tools offered by ONE of YOUR configured per-user MCP servers. " +
			"Connects to the server live, runs `tools/list`, and returns every tool's " +
			"name, description, and input_schema. This is the AUTHORITATIVE way to learn " +
			"a server's exact tool names and argument shapes; always call it before " +
			"`mcp_call` for a server whose tools you do not yet know. Never guess a tool " +
			"name or argument shape — a wrong guess is rejected before the remote call " +
			"is made. The discovered tools are written back to your config cache in the " +
			"background so later `mcp_call` validations pass without a refresh.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"server": {
					"type": "string",
					"description": "The configured server name (see mcp_list_servers) whose tools you want to discover."
				}
			},
			"required": ["server"],
			"additionalProperties": false
		}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Server string `json:"server"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("mcp_list_tools: parse args: %w", err)
			}
			return listTools(ctx, tools.manager, a.Server)
		},
	}
	return r.Register(spec)
}

// addResult is the redacted confirmation returned by mcp_add_server. It never
// carries the auth value.
type addResult struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Status string `json:"status"`
	Tools  int    `json:"tools"`
	Auth   redactedAuth `json:"auth"`
}

// removeResult is the redacted confirmation returned by mcp_remove_server.
type removeResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// callResult mirrors mcpclient.ToolsCallResult so the agent receives the remote
// content directly; auth never appears here (it is injected server-side).
type callResult struct {
	Content []mcpclient.Content `json:"content"`
}

// listServers returns the redacted server list for the manager's workspace.
func listServers(m *Manager) ([]serverView, error) {
	cfg, err := LoadConfig(m.WorkspaceRoot())
	if err != nil {
		return nil, err
	}
	out := make([]serverView, 0, len(cfg.Servers))
	for _, s := range cfg.SortedServers() {
		out = append(out, serverViewFrom(s))
	}
	return out, nil
}

// addServer validates the entry, connects to capture tools/list, and persists
// the per-server config. The connection is cached on the manager so the
// following mcp_call reuses it.
func addServer(ctx context.Context, m *Manager, name, url, description, transport string, auth Auth) (addResult, error) {
	// Name validation runs first with a full rule/example message so an invalid
	// name is rejected before any network activity.
	if err := ValidateName(name); err != nil {
		return addResult{}, fmt.Errorf("mcp_add_server: %w", err)
	}
	if strings.TrimSpace(url) == "" {
		return addResult{}, fmt.Errorf("mcp_add_server: url is required")
	}
	if transport == "" {
		transport = "http"
	}

	// Reject a duplicate before connecting so a re-add surfaces a clear error
	// without wasting a connection. The check is "directory already exists"
	// semantics over the loaded config.
	cfg, err := LoadConfig(m.WorkspaceRoot())
	if err != nil {
		return addResult{}, err
	}
	if _, ok := cfg.Server(name); ok {
		return addResult{}, fmt.Errorf("mcp_add_server: server %q already exists", name)
	}

	server := Server{
		Name:        name,
		URL:         url,
		Transport:   transport,
		Auth:        auth,
		Description: description,
	}

	// Validate the server fields (transport/auth/url) before reaching out so a
	// misconfigured entry is rejected without a wasted connection.
	if err := validateServer(server); err != nil {
		return addResult{}, fmt.Errorf("mcp_add_server: %w", err)
	}

	// Connect to validate reachability and capture the tools/list cache. This
	// also warms the manager's connection for the turn.
	tools, err := m.ConnectServer(ctx, server)
	if err != nil {
		return addResult{}, fmt.Errorf("mcp_add_server: %w", err)
	}
	server.Tools = freshToCache(tools)

	// Persist this single server to its own config file. WriteServer (via
	// AddServer) re-checks the name and the directory-uniqueness to close the
	// check-then-act window against a concurrent add of the same name.
	if err := AddServer(m.WorkspaceRoot(), server); err != nil {
		return addResult{}, err
	}
	return addResult{
		Name:   name,
		URL:    url,
		Status: "added",
		Tools:  len(tools),
		Auth:   redactAuth(auth),
	}, nil
}

// removeServer drops the named server's directory (leaving every other server
// untouched) and drops the cached connection so a later re-add reconnects
// cleanly.
func removeServer(m *Manager, name string) (removeResult, error) {
	removed, err := RemoveServer(m.WorkspaceRoot(), name)
	if err != nil {
		return removeResult{}, err
	}
	if !removed {
		return removeResult{}, fmt.Errorf("mcp_remove_server: server %q is not configured", name)
	}
	m.DropConnection(name)
	return removeResult{Name: name, Status: "removed"}, nil
}

// callTool validates the tool/args against the cached schema, then invokes the
// remote tool on the turn-scoped connection. The whole operation is bounded by
// the total-call timeout.
func callTool(ctx context.Context, m *Manager, serverName, toolName string, args json.RawMessage) (callResult, error) {
	totalCtx, cancel := context.WithTimeout(ctx, m.callTimeout)
	defer cancel()

	cfg, err := LoadConfig(m.WorkspaceRoot())
	if err != nil {
		return callResult{}, err
	}
	server, ok := cfg.Server(serverName)
	if !ok {
		return callResult{}, fmt.Errorf("mcp_call: server %q is not configured", serverName)
	}

	// Validate the tool name and args against the persisted cache. If the tool
	// is absent, attempt one tools/list refresh (task 4.3) in case the server
	// changed since mcp_add_server.
	schema, ok := lookupToolSchema(server.Tools, toolName)
	if !ok {
		fresh, refreshErr := m.ListServerTools(totalCtx, serverName)
		if refreshErr == nil {
			// Update the persisted cache and re-resolve.
			if err := persistRefreshedTools(m, serverName, fresh); err == nil {
				server.Tools = freshToCache(fresh)
			}
			schema, ok = lookupToolSchema(server.Tools, toolName)
		}
		if !ok {
			return callResult{}, fmt.Errorf("mcp_call: tool %q is not advertised by server %q", toolName, serverName)
		}
	}
	if err := validateArgs(schema, args); err != nil {
		return callResult{}, fmt.Errorf("mcp_call: args invalid: %w", err)
	}

	c, err := m.Conn(totalCtx, serverName)
	if err != nil {
		return callResult{}, err
	}
	result, err := c.Call(totalCtx, toolName, args)
	if err != nil {
		return callResult{}, err
	}
	return callResult{Content: result.Content}, nil
}

// lookupToolSchema returns the cached input schema for toolName (ok=false when
// the tool is not in the cache).
func lookupToolSchema(tools []ToolCache, toolName string) (json.RawMessage, bool) {
	for _, t := range tools {
		if t.Name == toolName {
			return t.InputSchema, true
		}
	}
	return nil, false
}

// persistRefreshedTools rewrites the named server's tool cache in its own
// config file so subsequent calls skip the refresh.
func persistRefreshedTools(m *Manager, serverName string, tools []mcpclient.Tool) error {
	cfg, err := LoadConfig(m.WorkspaceRoot())
	if err != nil {
		return err
	}
	server, ok := cfg.Server(serverName)
	if !ok {
		return fmt.Errorf("mcp server %q is not configured", serverName)
	}
	server.Tools = freshToCache(tools)
	return WriteServer(m.WorkspaceRoot(), server)
}

// freshToCache converts a fresh tools/list snapshot into the persisted cache
// shape.
func freshToCache(tools []mcpclient.Tool) []ToolCache {
	out := make([]ToolCache, 0, len(tools))
	for _, t := range tools {
		out = append(out, ToolCache{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out
}

// writeBackTimeout bounds the async cache write-back launched by mcp_list_tools.
// The cache write is small local file I/O (LoadConfig + WriteServer) which Go's
// os package does not make context-cancelable; this constant documents the
// intended bound and the background context keeps the write independent of the
// turn's lifetime so it survives turn cancellation.
const writeBackTimeout = 5 * time.Second

// toolView is one entry in the mcp_list_tools result: a discovered tool's
// name/description/input_schema. It mirrors ToolCache's wire shape but is a
// distinct type so the discovery result is semantically separate from the
// persisted cache.
type toolView struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// listTools connects to serverName live, runs tools/list, and returns the
// server's tools as discovery views. The server must already be configured;
// unknown server, connect failure, or tools/list failure surfaces a clear
// error and triggers no write-back. On success the fresh list is written back
// to the server's config cache asynchronously (fire-and-forget) AFTER the
// result is computed, so the return is never blocked by the write.
func listTools(ctx context.Context, m *Manager, serverName string) ([]toolView, error) {
	// Validate the server is configured before connecting so an unknown name
	// is rejected with a clear error and no network activity (and no
	// write-back).
	cfg, err := LoadConfig(m.WorkspaceRoot())
	if err != nil {
		return nil, err
	}
	if _, ok := cfg.Server(serverName); !ok {
		return nil, fmt.Errorf("mcp_list_tools: server %q is not configured", serverName)
	}

	fresh, err := m.ListServerTools(ctx, serverName)
	if err != nil {
		return nil, fmt.Errorf("mcp_list_tools: %w", err)
	}

	views := toolsToViews(fresh)
	// Trigger the async cache write-back AFTER the result is computed. It is
	// fire-and-forget (independent context, value copies, no Manager
	// reference), so it never blocks this return or affects the turn.
	writeBackToolsAsync(m.WorkspaceRoot(), serverName, fresh)
	return views, nil
}

// toolsToViews projects a fresh tools/list snapshot into the discovery view
// shape returned to the agent.
func toolsToViews(tools []mcpclient.Tool) []toolView {
	out := make([]toolView, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolView{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out
}

// writeBackToolsAsync writes the freshly discovered tools for serverName back
// to its per-server config cache in a fire-and-forget goroutine. It uses an
// independent context (context.Background() capped at writeBackTimeout) so it
// survives turn cancellation, captures only value copies of its inputs (never
// the turn-scoped Manager or caller-owned mutable state), and never blocks or
// fails the caller: a write error is logged as a warning and dropped.
func writeBackToolsAsync(workspaceRoot, serverName string, tools []mcpclient.Tool) {
	// Value-copy the tool list (deep-copying the InputSchema bytes) so the
	// goroutine holds no reference to the caller's slice or the turn-scoped
	// Manager. The caller may return and the turn may end (Manager.Close'd)
	// before this goroutine finishes.
	cache := deepCopyToolCache(freshToCache(tools))
	go func() {
		// ctx is independent of the turn so the write survives turn
		// cancellation. It bounds the goroutine's logical lifetime; the cache
		// write itself is plain file I/O that completes promptly.
		ctx, cancel := context.WithTimeout(context.Background(), writeBackTimeout)
		defer cancel()
		if err := persistServerToolsCache(ctx, workspaceRoot, serverName, cache); err != nil {
			logger.L().Warn("mcp_list_tools: async cache write-back failed (non-fatal)",
				zap.String("server", serverName),
				zap.Error(err))
		}
	}()
}

// persistServerToolsCache rewrites serverName's tool cache in its own config
// file, preserving the server's other fields (url/transport/auth/description).
// It is the Manager-free variant of persistRefreshedTools for the async
// write-back path (the goroutine must not reference the turn-scoped Manager).
// ctx bounds the operation's logical lifetime; the underlying file I/O is not
// context-cancelable, so a cancelled ctx is checked up front and the small
// local write otherwise runs to completion.
func persistServerToolsCache(ctx context.Context, workspaceRoot, serverName string, tools []ToolCache) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := LoadConfig(workspaceRoot)
	if err != nil {
		return err
	}
	server, ok := cfg.Server(serverName)
	if !ok {
		return fmt.Errorf("mcp server %q is not configured", serverName)
	}
	server.Tools = tools
	return WriteServer(workspaceRoot, server)
}

// deepCopyToolCache returns a deep copy of in so the result does not alias the
// caller's slice or its InputSchema backing bytes. Used by the async write-back
// to guarantee the goroutine captures only value copies.
func deepCopyToolCache(in []ToolCache) []ToolCache {
	out := make([]ToolCache, len(in))
	for i, t := range in {
		out[i] = ToolCache{Name: t.Name, Description: t.Description}
		if len(t.InputSchema) > 0 {
			cp := make([]byte, len(t.InputSchema))
			copy(cp, t.InputSchema)
			out[i].InputSchema = cp
		}
	}
	return out
}
