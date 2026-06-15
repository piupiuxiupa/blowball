package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"
)

// RegisterAll connects every configured MCP server, discovers its tools, and
// registers proxy ToolSpecs into reg. The returned closer releases all
// underlying transports; callers should invoke it on shutdown.
func RegisterAll(ctx context.Context, reg *tool.Registry, cfg config.MCPConfig) (func() error, error) {
	clients := make([]*Client, 0, len(cfg.Servers))
	closeAll := func() error {
		var first error
		for _, c := range clients {
			if err := c.Close(); err != nil && first == nil {
				first = err
			}
		}
		return first
	}

	for _, sc := range cfg.Servers {
		client, err := connectServer(sc)
		if err != nil {
			_ = closeAll()
			return nil, err
		}
		clients = append(clients, client)

		if err := registerServerTools(reg, client); err != nil {
			_ = closeAll()
			return nil, err
		}
	}

	return closeAll, nil
}

// TransportFactory builds a Transport for a server config. Tests can override
// it to inject mocks without touching the public API.
var TransportFactory = func(sc config.MCPServerConfig) (Transport, error) {
	switch sc.Transport {
	case "sse":
		return NewSSETransport(sc.URL, sc.Headers, sc.Timeout), nil
	case "stdio":
		return NewStdioTransport(sc.Command, sc.Args, sc.Env), nil
	default:
		return nil, fmt.Errorf("unsupported transport %q", sc.Transport)
	}
}

func connectServer(sc config.MCPServerConfig) (*Client, error) {
	transport, err := TransportFactory(sc)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", sc.Name, err)
	}

	client, err := NewClient(sc.Name, sc.Prefix, transport, sc.Timeout, sc.CallTimeout)
	if err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("mcp server %q: %w", sc.Name, err)
	}
	return client, nil
}

func registerServerTools(reg *tool.Registry, client *Client) error {
	for _, remote := range client.Tools() {
		name := client.PrefixedName(remote.Name)
		if name == "" {
			continue
		}

		// Check for collisions before attempting registration so we can report
		// the offending server and original tool name.
		if existing, ok := reg.Get(name); ok {
			return fmt.Errorf("mcp server %q: tool name collision %q already registered by %q",
				client.Name(), name, existing.Name)
		}

		remote := remote
		execute := func(ctx context.Context, args json.RawMessage) (any, error) {
			result, err := client.CallTool(ctx, remote.Name, args)
			if err != nil {
				return nil, err
			}
			if result.IsError {
				return nil, toolResultError(result)
			}
			return result, nil
		}

		description := remote.Description
		if description == "" {
			description = fmt.Sprintf("Proxy tool %q from MCP server %q", remote.Name, client.Name())
		}

		if err := reg.Register(&tool.ToolSpec{
			Name:           name,
			Description:    description,
			ParametersJSON: normalizeSchema(remote.InputSchema),
			Execute:        execute,
		}); err != nil {
			return fmt.Errorf("mcp server %q: register %q: %w", client.Name(), name, err)
		}
	}
	return nil
}

func toolResultError(result *ToolsCallResult) error {
	var parts []string
	for _, c := range result.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	if len(parts) == 0 {
		return fmt.Errorf("remote tool returned an error")
	}
	return fmt.Errorf("remote tool error: %s", strings.Join(parts, "; "))
}

func normalizeSchema(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 {
		return json.RawMessage(`{}`)
	}
	var v any
	if err := json.Unmarshal(schema, &v); err != nil {
		return json.RawMessage(`{}`)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
