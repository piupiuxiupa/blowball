package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClient_NewClient_Success(t *testing.T) {
	mt := &mockTransport{
		initResult: &InitializeResult{ProtocolVersion: "2024-11-05"},
		tools: []Tool{
			{Name: "add", Description: "adds", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}

	client, err := NewClient("calc", "", mt, time.Second, time.Second)
	require.NoError(t, err)
	require.Equal(t, "calc", client.Name())
	require.Len(t, client.Tools(), 1)
	require.Equal(t, "add", client.Tools()[0].Name)

	require.NoError(t, client.Close())
	require.True(t, mt.closed)
}

func TestClient_NewClient_InitFailure(t *testing.T) {
	mt := &mockTransport{initErr: errors.New("init failed")}
	_, err := NewClient("calc", "", mt, time.Second, time.Second)
	require.Error(t, err)
}

func TestClient_NewClient_ListToolsFailure(t *testing.T) {
	mt := &mockTransport{
		initResult: &InitializeResult{ProtocolVersion: "2024-11-05"},
		listErr:    errors.New("list failed"),
	}
	_, err := NewClient("calc", "", mt, time.Second, time.Second)
	require.Error(t, err)
}

func TestClient_PrefixedName(t *testing.T) {
	mt := &mockTransport{
		initResult: &InitializeResult{ProtocolVersion: "2024-11-05"},
		tools:      []Tool{{Name: "add"}},
	}
	client, err := NewClient("calc", "remote_", mt, time.Second, time.Second)
	require.NoError(t, err)
	require.Equal(t, "remote_add", client.PrefixedName("add"))
}

func TestClient_CallTool(t *testing.T) {
	mt := &mockTransport{
		initResult: &InitializeResult{ProtocolVersion: "2024-11-05"},
		tools:      []Tool{{Name: "add"}},
		callResult: &ToolsCallResult{Content: []Content{{Type: "text", Text: "42"}}},
	}
	client, err := NewClient("calc", "", mt, time.Second, time.Second)
	require.NoError(t, err)

	ctx := context.Background()
	result, err := client.CallTool(ctx, "add", json.RawMessage(`{"a":1}`))
	require.NoError(t, err)
	require.Len(t, result.Content, 1)
	require.Equal(t, "42", result.Content[0].Text)
}
