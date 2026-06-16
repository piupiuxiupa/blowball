package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"

	"github.com/stretchr/testify/require"
)

func patchTransportFactory(t *testing.T, tr Transport) {
	t.Helper()
	old := TransportFactory
	TransportFactory = func(sc config.MCPServerConfig) (Transport, error) {
		return tr, nil
	}
	t.Cleanup(func() { TransportFactory = old })
}

func TestRegisterAll_Success(t *testing.T) {
	reg := tool.NewRegistry()

	mt := &mockTransport{
		initResult: &InitializeResult{ProtocolVersion: "2024-11-05"},
		tools: []Tool{
			{Name: "add", Description: "adds numbers", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		callResult: &ToolsCallResult{Content: []Content{{Type: "text", Text: "3"}}},
	}
	patchTransportFactory(t, mt)

	closeAll, err := RegisterAll(context.Background(), reg, config.MCPConfig{
		Servers: []config.MCPServerConfig{{
			Name:      "calc",
			Transport: "sse",
		}},
	})
	require.NoError(t, err)

	spec, ok := reg.Get("add")
	require.True(t, ok)
	require.Equal(t, "adds numbers", spec.Description)

	result, err := spec.Execute(context.Background(), json.RawMessage(`{"a":1,"b":2}`))
	require.NoError(t, err)
	require.Equal(t, &ToolsCallResult{Content: []Content{{Type: "text", Text: "3"}}}, result)

	require.NoError(t, closeAll())
	require.True(t, mt.closed)
}

func TestRegisterAll_Prefix(t *testing.T) {
	reg := tool.NewRegistry()

	mt := &mockTransport{
		initResult: &InitializeResult{ProtocolVersion: "2024-11-05"},
		tools:      []Tool{{Name: "add"}},
	}
	patchTransportFactory(t, mt)

	_, err := RegisterAll(context.Background(), reg, config.MCPConfig{
		Servers: []config.MCPServerConfig{{
			Name:      "calc",
			Transport: "sse",
			Prefix:    "remote_",
		}},
	})
	require.NoError(t, err)

	_, ok := reg.Get("remote_add")
	require.True(t, ok)
	_, ok = reg.Get("add")
	require.False(t, ok)
}

func TestRegisterAll_Collision(t *testing.T) {
	reg := tool.NewRegistry()
	require.NoError(t, reg.Register(&tool.ToolSpec{Name: "add", Execute: func(context.Context, json.RawMessage) (any, error) { return nil, nil }}))

	mt := &mockTransport{
		initResult: &InitializeResult{ProtocolVersion: "2024-11-05"},
		tools:      []Tool{{Name: "add"}},
	}
	patchTransportFactory(t, mt)

	_, err := RegisterAll(context.Background(), reg, config.MCPConfig{
		Servers: []config.MCPServerConfig{{
			Name:      "calc",
			Transport: "sse",
		}},
	})
	require.Error(t, err)
}

func TestRegisterAll_InitFailure(t *testing.T) {
	reg := tool.NewRegistry()
	mt := &mockTransport{initErr: errors.New("boom")}
	patchTransportFactory(t, mt)

	_, err := RegisterAll(context.Background(), reg, config.MCPConfig{
		Servers: []config.MCPServerConfig{{
			Name:      "calc",
			Transport: "sse",
		}},
	})
	require.Error(t, err)
}

func TestRegisterAllWithManager_Ownership(t *testing.T) {
	reg := tool.NewRegistry()

	mt := &mockTransport{
		initResult: &InitializeResult{ProtocolVersion: "2024-11-05"},
		tools: []Tool{
			{Name: "add", Description: "adds"},
			{Name: "sub", Description: "subs"},
		},
	}
	patchTransportFactory(t, mt)

	mgr, err := RegisterAllWithManager(context.Background(), reg, config.MCPConfig{
		Servers: []config.MCPServerConfig{{
			Name:      "calc",
			Transport: "sse",
			Prefix:    "remote_",
		}},
	})
	require.NoError(t, err)
	defer func() { _ = mgr.Close() }()

	serverTools := mgr.ServerTools()
	require.Len(t, serverTools["calc"], 2)
	require.Contains(t, serverTools["calc"], "remote_add")
	require.Contains(t, serverTools["calc"], "remote_sub")

	_, ok := reg.Get("remote_add")
	require.True(t, ok)
}

func TestRegisterAll_BackwardCompatible(t *testing.T) {
	reg := tool.NewRegistry()

	mt := &mockTransport{
		initResult: &InitializeResult{ProtocolVersion: "2024-11-05"},
		tools:      []Tool{{Name: "add"}},
	}
	patchTransportFactory(t, mt)

	closeAll, err := RegisterAll(context.Background(), reg, config.MCPConfig{
		Servers: []config.MCPServerConfig{{
			Name:      "calc",
			Transport: "sse",
		}},
	})
	require.NoError(t, err)
	require.NoError(t, closeAll())
	require.True(t, mt.closed)
}

func TestRegisterAll_ToolError(t *testing.T) {
	reg := tool.NewRegistry()

	mt := &mockTransport{
		initResult: &InitializeResult{ProtocolVersion: "2024-11-05"},
		tools:      []Tool{{Name: "add"}},
		callResult: &ToolsCallResult{IsError: true, Content: []Content{{Type: "text", Text: "bad args"}}},
	}
	patchTransportFactory(t, mt)

	_, err := RegisterAll(context.Background(), reg, config.MCPConfig{
		Servers: []config.MCPServerConfig{{
			Name:      "calc",
			Transport: "sse",
		}},
	})
	require.NoError(t, err)

	spec, ok := reg.Get("add")
	require.True(t, ok)

	_, err = spec.Execute(context.Background(), json.RawMessage(`{}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad args")
}
