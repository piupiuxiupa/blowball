package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/mcpclient"
	"github.com/lush/blowball/internal/tool/skill"
)

// Registered tool names. These are the strings agents reference in their
// config `tools:` lists.
const (
	ToolListServers  = "mcp_list_servers"
	ToolAddServer    = "mcp_add_server"
	ToolRemoveServer = "mcp_remove_server"
	ToolCall         = "mcp_call"
)

// IsMCPTool reports whether name is one of the per-user mcp_* tools.
func IsMCPTool(name string) bool {
	switch name {
	case ToolListServers, ToolAddServer, ToolRemoveServer, ToolCall:
		return true
	}
	return false
}

// mcpTools is the static list of per-user mcp_* tool names, used by the
// orchestrator wiring to decide whether an agent uses the family.
var mcpTools = []string{ToolListServers, ToolAddServer, ToolRemoveServer, ToolCall}

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

// RegisterAll registers the four mcp_* tools into r.
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
	return nil
}

// registerListServers: mcp_list_servers — list the caller's configured servers
// with credentials redacted (leak invariant #1).
func registerListServers(r *tool.Registry, tools *Tools) error {
	spec := &tool.ToolSpec{
		Name: ToolListServers,
		Description: "List the per-user MCP servers configured in YOUR workspace " +
			"(`.blowball/mcp/config.json`). Returns name, url, transport, description, " +
			"the auth KIND (credentials are redacted), and how many tools each server " +
			"advertises. Use this to discover which servers you can call with `mcp_call`. " +
			"Credentials are never shown.",
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
					"description": "A short, unique server identifier (simple name, not a path)."
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
// the config. The connection is cached on the manager so the following
// mcp_call reuses it.
func addServer(ctx context.Context, m *Manager, name, url, description, transport string, auth Auth) (addResult, error) {
	if strings.TrimSpace(name) == "" {
		return addResult{}, fmt.Errorf("mcp_add_server: name is required")
	}
	if strings.TrimSpace(url) == "" {
		return addResult{}, fmt.Errorf("mcp_add_server: url is required")
	}
	if transport == "" {
		transport = "http"
	}
	server := Server{
		Name:        name,
		URL:         url,
		Transport:   transport,
		Auth:        auth,
		Description: description,
	}

	cfg, err := LoadConfig(m.WorkspaceRoot())
	if err != nil {
		return addResult{}, err
	}
	if err := cfg.AddServer(server); err != nil {
		return addResult{}, err
	}

	// Connect to validate reachability and capture the tools/list cache. This
	// also warms the manager's connection for the turn.
	tools, err := m.ConnectServer(ctx, server)
	if err != nil {
		return addResult{}, fmt.Errorf("mcp_add_server: %w", err)
	}
	for _, t := range tools {
		cfg.Servers[len(cfg.Servers)-1].Tools = append(cfg.Servers[len(cfg.Servers)-1].Tools, ToolCache{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	if err := WriteConfig(m.WorkspaceRoot(), cfg); err != nil {
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

// removeServer drops the named server and persists; it also drops the cached
// connection so a later re-add reconnects cleanly.
func removeServer(m *Manager, name string) (removeResult, error) {
	cfg, err := LoadConfig(m.WorkspaceRoot())
	if err != nil {
		return removeResult{}, err
	}
	if !cfg.RemoveServer(name) {
		return removeResult{}, fmt.Errorf("mcp_remove_server: server %q is not configured", name)
	}
	if err := WriteConfig(m.WorkspaceRoot(), cfg); err != nil {
		return removeResult{}, err
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

// persistRefreshedTools rewrites the named server's tool cache in the config
// file so subsequent calls skip the refresh.
func persistRefreshedTools(m *Manager, serverName string, tools []mcpclient.Tool) error {
	cfg, err := LoadConfig(m.WorkspaceRoot())
	if err != nil {
		return err
	}
	for i := range cfg.Servers {
		if cfg.Servers[i].Name != serverName {
			continue
		}
		cfg.Servers[i].Tools = freshToCache(tools)
		break
	}
	return WriteConfig(m.WorkspaceRoot(), cfg)
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
