package mcpclient

import (
	"context"
	"fmt"
	"time"
)

// default timeouts when the config leaves them at zero.
const (
	defaultTimeout     = 30 * time.Second
	defaultCallTimeout = 30 * time.Second
)

// Client wraps a Transport and caches the discovered tool list.
type Client struct {
	transport   Transport
	name        string
	prefix      string
	callTimeout time.Duration
	tools       []Tool
}

// NewClient initializes a Client by connecting to server, performing the MCP
// handshake, and fetching the tool list. The transport is owned by the client
// and closed by Close.
func NewClient(serverName, prefix string, transport Transport, timeout, callTimeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if callTimeout <= 0 {
		callTimeout = defaultCallTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	_, err := transport.Initialize(ctx, InitializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities:    map[string]any{},
		ClientInfo:      ClientInfo{Name: "blowball", Version: "0.1.0"},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize server %q: %w", serverName, err)
	}

	listCtx, listCancel := context.WithTimeout(context.Background(), timeout)
	defer listCancel()
	list, err := transport.ListTools(listCtx)
	if err != nil {
		return nil, fmt.Errorf("list tools for server %q: %w", serverName, err)
	}

	return &Client{
		transport:   transport,
		name:        serverName,
		prefix:      prefix,
		callTimeout: callTimeout,
		tools:       list.Tools,
	}, nil
}

// Name returns the configured server name.
func (c *Client) Name() string { return c.name }

// Prefix returns the configured tool-name prefix.
func (c *Client) Prefix() string { return c.prefix }

// Tools returns the cached tool list discovered at initialization.
func (c *Client) Tools() []Tool {
	out := make([]Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

// PrefixedName returns name with the server prefix applied, if any.
func (c *Client) PrefixedName(name string) string {
	if c.prefix == "" {
		return name
	}
	return c.prefix + name
}

// CallTool invokes the named remote tool with the given arguments.
func (c *Client) CallTool(ctx context.Context, name string, arguments []byte) (*ToolsCallResult, error) {
	ctx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()

	return c.transport.CallTool(ctx, ToolsCallParams{
		Name:      name,
		Arguments: arguments,
	})
}

// Close releases the underlying transport.
func (c *Client) Close() error {
	if c.transport == nil {
		return nil
	}
	return c.transport.Close()
}
