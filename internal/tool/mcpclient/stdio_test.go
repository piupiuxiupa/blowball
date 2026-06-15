package mcpclient

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func stdioServerPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(file), "testdata", "stdio_server.go")
}

func TestStdioTransport_Initialize(t *testing.T) {
	tr := NewStdioTransport("go", []string{"run", stdioServerPath(t)}, nil)
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := tr.Initialize(ctx, InitializeParams{ProtocolVersion: "2024-11-05"})
	require.NoError(t, err)
	require.Equal(t, "2024-11-05", res.ProtocolVersion)
}

func TestStdioTransport_ListTools(t *testing.T) {
	tr := NewStdioTransport("go", []string{"run", stdioServerPath(t)}, nil)
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := tr.Initialize(ctx, InitializeParams{ProtocolVersion: "2024-11-05"})
	require.NoError(t, err)

	list, err := tr.ListTools(ctx)
	require.NoError(t, err)
	require.Len(t, list.Tools, 1)
	require.Equal(t, "add", list.Tools[0].Name)
}

func TestStdioTransport_CallTool(t *testing.T) {
	tr := NewStdioTransport("go", []string{"run", stdioServerPath(t)}, nil)
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := tr.Initialize(ctx, InitializeParams{ProtocolVersion: "2024-11-05"})
	require.NoError(t, err)

	res, err := tr.CallTool(ctx, ToolsCallParams{Name: "add", Arguments: json.RawMessage(`{"a":1}`)})
	require.NoError(t, err)
	require.Len(t, res.Content, 1)
	require.Equal(t, "done", res.Content[0].Text)
}

// time import is referenced by timeout defaults.
var _ = time.Second
