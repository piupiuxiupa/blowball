package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/mcp"
)

// MCPHandler owns the /api/v1/mcp/* routes. The Tools endpoint returns ONLY
// MCP-sourced tools: operator (global) MCP proxy tools registered in the
// process registry, plus the caller's per-user MCP servers' cached tools.
// Built-in tools (xizhi_*/webfetch/executor/luban_*) and synthetic invoke_*
// dispatch tools are excluded.
//
// The endpoint is cache-based and makes no MCP connections: the operator view
// comes from the registry + the server ownership map (captured at startup),
// and the per-user view comes from the caller's on-disk config cache. Cache
// freshness is maintained by the agent-runtime writers (mcp_list_tools
// write-back, mcp_add_server, mcp_call cache-miss refresh).
type MCPHandler struct {
	reg *tool.Registry
	// serverForTool maps an operator MCP proxy tool name to its owning
	// operator server name. Built from mcpclient.Manager.ServerTools() at
	// startup; only registry tools present in this map are operator MCP proxy
	// tools (everything else is a built-in to exclude).
	serverForTool map[string]string
	// workspaceRootForUser resolves the authenticated user id to its workspace
	// root, used to read the per-user MCP config cache. Agent-only wiring.
	workspaceRootForUser func(userID string) string
}

// NewMCPHandler wires the handler with the tool registry, the operator MCP
// server→tools ownership map (mcpclient.Manager.ServerTools()), and the
// userID→workspace-root resolver used to read the caller's per-user MCP cache.
func NewMCPHandler(reg *tool.Registry, serverTools map[string][]string, workspaceRootForUser func(userID string) string) *MCPHandler {
	// Invert server→[]tool into tool→server for O(1) lookup of a registry
	// tool's owning operator server.
	serverForTool := make(map[string]string)
	for server, names := range serverTools {
		for _, n := range names {
			serverForTool[n] = server
		}
	}
	return &MCPHandler{
		reg:                  reg,
		serverForTool:        serverForTool,
		workspaceRootForUser: workspaceRootForUser,
	}
}

// mcpTool is the wire shape of one entry in the MCP tools response. It mirrors
// the OpenAI function-tool layout the agent loop consumes, with an added
// `server` field attributing the tool to its MCP source (operator server name
// or per-user server name).
type mcpTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Server      string          `json:"server"`
}

// Tools handles GET /api/v1/mcp/tools. Returns 200 with only MCP-sourced tools
// (operator proxy + per-user cached), each attributed to its server. Read-only;
// makes no MCP connections. Missing/empty per-user config yields only the
// operator tools without error; a single malformed per-user server is omitted
// (by LoadConfig's enumeration) while the rest still return.
func (h *MCPHandler) Tools(c *gin.Context) {
	tools := make([]mcpTool, 0, 8)

	// Operator (global) MCP proxy tools: keep only registry entries owned by a
	// configured operator MCP server. Built-in tools (xizhi_*, webfetch, ...) and
	// synthetic invoke_* tools have no entry in serverForTool and are excluded.
	// reg.List() is name-sorted, so the operator block is deterministic.
	for _, spec := range h.reg.List() {
		server, ok := h.serverForTool[spec.Name]
		if !ok {
			continue
		}
		params := spec.ParametersJSON
		if len(params) == 0 {
			params = json.RawMessage(`{}`)
		}
		tools = append(tools, mcpTool{
			Type:        "function",
			Name:        spec.Name,
			Description: spec.Description,
			Parameters:  params,
			Server:      server,
		})
	}

	// Per-user MCP: read the caller's config cache (no network) and flatten each
	// server's cached tools. The caller's userID is derived from the JWT;
	// workspaceRootForUser resolves it to the workspace holding .blowball/mcp/.
	// A missing resolver/userID/config yields nothing (no error); LoadConfig
	// already skips malformed single servers.
	if h.workspaceRootForUser != nil {
		if userID := middleware.UserIDFromCtx(c); userID != "" {
			ws := h.workspaceRootForUser(userID)
			if cfg, err := mcp.LoadConfig(ws); err == nil && cfg != nil {
				for _, s := range cfg.SortedServers() {
					for _, t := range s.Tools {
						schema := t.InputSchema
						if len(schema) == 0 {
							schema = json.RawMessage(`{}`)
						}
						tools = append(tools, mcpTool{
							Type:        "function",
							Name:        t.Name,
							Description: t.Description,
							Parameters:  schema,
							Server:      s.Name,
						})
					}
				}
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"tools": tools})
}
