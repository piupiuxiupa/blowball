package mcpclient

import "context"

// Transport is the MCP transport abstraction. Implementations must be safe for
// concurrent use because multiple agent turns may call CallTool in parallel.
type Transport interface {
	// Initialize performs the MCP initialize handshake.
	Initialize(ctx context.Context, params InitializeParams) (*InitializeResult, error)

	// ListTools fetches the remote tool catalogue.
	ListTools(ctx context.Context) (*ToolsListResult, error)

	// CallTool invokes a remote tool with the given arguments.
	CallTool(ctx context.Context, params ToolsCallParams) (*ToolsCallResult, error)

	// Close releases the underlying connection or process.
	Close() error
}
