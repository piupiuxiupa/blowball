package mcpclient

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strconv"
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

// TestStdioTransport_LargeMessage guards against the regression where a single
// newline-delimited JSON-RPC line over the prior 1 MiB bufio.Scanner cap
// failed with `bufio.Scanner: token too long`. The test server emits a 2 MiB
// result via the STDIO_BIG_RESULT_BYTES env hook.
func TestStdioTransport_LargeMessage(t *testing.T) {
	tr := NewStdioTransport("go", []string{"run", stdioServerPath(t)}, map[string]string{
		"STDIO_BIG_RESULT_BYTES": strconv.Itoa(2 << 20), // 2 MiB on one line
	})
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := tr.Initialize(ctx, InitializeParams{ProtocolVersion: "2024-11-05"})
	require.NoError(t, err)

	res, err := tr.CallTool(ctx, ToolsCallParams{Name: "add", Arguments: json.RawMessage(`{"a":1}`)})
	require.NoError(t, err)
	require.Len(t, res.Content, 1)
	require.Len(t, res.Content[0].Text, 2<<20)
}
