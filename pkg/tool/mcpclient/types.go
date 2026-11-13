package mcpclient

import (
	"encoding/json"
	"fmt"
)

// JSON-RPC 2.0 request/response envelope types used by the MCP protocol.

const jsonRPCVersion = "2.0"

// Request is a JSON-RPC request object.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// Response is a JSON-RPC response object.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ErrorObject    `json:"error,omitempty"`
}

// ErrorObject is a JSON-RPC error object.
type ErrorObject struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *ErrorObject) Error() string {
	if e == nil {
		return "nil mcp error"
	}
	return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message)
}

// InitializeParams is the params for the initialize request.
type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      ClientInfo     `json:"clientInfo"`
}

// ClientInfo identifies this MCP client.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult is the result of a successful initialize call.
type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      ServerInfo     `json:"serverInfo"`
}

// ServerInfo identifies the remote MCP server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ToolsListResult is the result of a tools/list call.
type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

// Tool describes a remote tool returned by tools/list.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ToolsCallParams is the params for a tools/call request.
type ToolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolsCallResult is the result of a tools/call request.
type ToolsCallResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// Content is a single content item in a tool call result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// newRequest builds a Request with the next sequential id.
func newRequest(id int, method string, params any) (*Request, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal %s params: %w", method, err)
	}
	return &Request{
		JSONRPC: jsonRPCVersion,
		ID:      id,
		Method:  method,
		Params:  raw,
	}, nil
}

// extractResult unmarshals resp.Result into v, returning a JSON-RPC error if
// the response carried one.
func extractResult(resp *Response, v any) error {
	if resp == nil {
		return fmt.Errorf("nil response")
	}
	if resp.Error != nil {
		return resp.Error
	}
	if err := json.Unmarshal(resp.Result, v); err != nil {
		return fmt.Errorf("unmarshal result: %w", err)
	}
	return nil
}
