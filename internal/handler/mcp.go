package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/tool"
)

// MCPHandler owns the /api/v1/mcp/* routes. Today only Tools is exposed; the
// route returns the combined tool catalogue (regular Xizhi tools registered in
// the registry plus the synthetic invoke_chongzhi / invoke_liang entries the
// Confuse loop dispatches).
type MCPHandler struct {
	reg *tool.Registry
}

// NewMCPHandler wires the handler with the tool registry.
func NewMCPHandler(reg *tool.Registry) *MCPHandler {
	return &MCPHandler{reg: reg}
}

// mcpTool is the wire shape of one entry in the MCP tools response. It mirrors
// the OpenAI function-tool layout the agent loop already consumes.
type mcpTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Tools handles GET /api/v1/mcp/tools. Returns 200 with the combined tool list
// (regular registry tools + invoke_chongzhi + invoke_liang). Read-only.
func (h *MCPHandler) Tools(c *gin.Context) {
	tools := make([]mcpTool, 0, 8)

	for _, spec := range h.reg.List() {
		params := spec.ParametersJSON
		if len(params) == 0 {
			params = json.RawMessage(`{}`)
		}
		tools = append(tools, mcpTool{
			Type:        "function",
			Name:        spec.Name,
			Description: spec.Description,
			Parameters:  params,
		})
	}

	for _, name := range []string{agent.ToolInvokeChongzhi, agent.ToolInvokeLiang} {
		params := agent.InvokeToolSchema(name)
		if params == nil {
			params = json.RawMessage(`{}`)
		}
		tools = append(tools, mcpTool{
			Type:        "function",
			Name:        name,
			Description: invokeDescription(name),
			Parameters:  params,
		})
	}

	c.JSON(http.StatusOK, gin.H{"tools": tools})
}

// invokeDescription returns the human-readable description Confuse uses for the
// named sub-agent invocation tool. It mirrors the strings in
// internal/agent/tools.go so the MCP catalogue matches what the agent loop
// actually presents to the model.
func invokeDescription(name string) string {
	switch name {
	case agent.ToolInvokeChongzhi:
		return "Invoke the Chongzhi (coding) sub-agent. Use for code editing, file writing, or any task that requires modifying files in the user's workspace."
	case agent.ToolInvokeLiang:
		return "Invoke the Liang (analysis) sub-agent. Use for analysis, explanation, or reasoning tasks that do not require file modification."
	}
	return ""
}
