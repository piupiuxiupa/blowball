package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHTTPTransport_Initialize_CachesSessionID(t *testing.T) {
	var gotInit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req Request
		require.NoError(t, json.Unmarshal(body, &req))

		switch req.Method {
		case "initialize":
			gotInit = true
			w.Header().Set(sessionHeader, "session-123")
			writeResponse(t, w, req.ID, InitializeResult{ProtocolVersion: "2024-11-05"})
		case "tools/list":
			require.Equal(t, "session-123", r.Header.Get(sessionHeader))
			writeResponse(t, w, req.ID, ToolsListResult{Tools: []Tool{{Name: "add"}}})
		case "tools/call":
			require.Equal(t, "session-123", r.Header.Get(sessionHeader))
			writeResponse(t, w, req.ID, ToolsCallResult{Content: []Content{{Type: "text", Text: "42"}}})
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL, nil, time.Second)
	defer tr.Close()

	ctx := context.Background()
	_, err := tr.Initialize(ctx, InitializeParams{ProtocolVersion: "2024-11-05"})
	require.NoError(t, err)
	require.True(t, gotInit)

	list, err := tr.ListTools(ctx)
	require.NoError(t, err)
	require.Len(t, list.Tools, 1)

	call, err := tr.CallTool(ctx, ToolsCallParams{Name: "add"})
	require.NoError(t, err)
	require.Equal(t, "42", call.Content[0].Text)
}

func TestHTTPTransport_NoSessionID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get(sessionHeader))
		body, _ := io.ReadAll(r.Body)
		var req Request
		require.NoError(t, json.Unmarshal(body, &req))
		switch req.Method {
		case "initialize":
			writeResponse(t, w, req.ID, InitializeResult{ProtocolVersion: "2024-11-05"})
		case "tools/list":
			writeResponse(t, w, req.ID, ToolsListResult{Tools: []Tool{{Name: "add"}}})
		}
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL, nil, time.Second)
	defer tr.Close()

	ctx := context.Background()
	_, err := tr.Initialize(ctx, InitializeParams{ProtocolVersion: "2024-11-05"})
	require.NoError(t, err)

	_, err = tr.ListTools(ctx)
	require.NoError(t, err)
}

func TestHTTPTransport_SSEResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "application/json, text/event-stream", r.Header.Get("Accept"))
		body, _ := io.ReadAll(r.Body)
		var req Request
		require.NoError(t, json.Unmarshal(body, &req))

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		raw, err := json.Marshal(Response{JSONRPC: jsonRPCVersion, ID: req.ID, Result: mustMarshal(InitializeResult{ProtocolVersion: "2024-11-05"})})
		require.NoError(t, err)
		fmt.Fprintf(w, "id:1\nevent:message\ndata:%s\n\n", string(raw))
		flusher.Flush()
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL, nil, time.Second)
	defer tr.Close()

	result, err := tr.Initialize(context.Background(), InitializeParams{ProtocolVersion: "2024-11-05"})
	require.NoError(t, err)
	require.Equal(t, "2024-11-05", result.ProtocolVersion)
}

func TestHTTPTransport_SessionExpirationRetry(t *testing.T) {
	initCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req Request
		require.NoError(t, json.Unmarshal(body, &req))
		switch req.Method {
		case "initialize":
			initCount++
			if initCount == 1 {
				w.Header().Set(sessionHeader, "session-1")
			} else {
				w.Header().Set(sessionHeader, "session-2")
			}
			writeResponse(t, w, req.ID, InitializeResult{ProtocolVersion: "2024-11-05"})
		case "tools/call":
			if r.Header.Get(sessionHeader) == "session-1" {
				writeError(t, w, req.ID, -32001, "session expired")
				return
			}
			require.Equal(t, "session-2", r.Header.Get(sessionHeader))
			writeResponse(t, w, req.ID, ToolsCallResult{Content: []Content{{Type: "text", Text: "ok"}}})
		}
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL, nil, time.Second)
	defer tr.Close()

	ctx := context.Background()
	_, err := tr.Initialize(ctx, InitializeParams{ProtocolVersion: "2024-11-05"})
	require.NoError(t, err)

	result, err := tr.CallTool(ctx, ToolsCallParams{Name: "add"})
	require.NoError(t, err)
	require.Equal(t, "ok", result.Content[0].Text)
	require.Equal(t, 2, initCount)
}

func TestHTTPTransport_RetryStillFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req Request
		require.NoError(t, json.Unmarshal(body, &req))
		switch req.Method {
		case "initialize":
			w.Header().Set(sessionHeader, "session-2")
			writeResponse(t, w, req.ID, InitializeResult{ProtocolVersion: "2024-11-05"})
		case "tools/call":
			writeError(t, w, req.ID, -32001, "session expired")
		}
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL, nil, time.Second)
	defer tr.Close()

	ctx := context.Background()
	_, err := tr.Initialize(ctx, InitializeParams{ProtocolVersion: "2024-11-05"})
	require.NoError(t, err)

	_, err = tr.CallTool(ctx, ToolsCallParams{Name: "add"})
	require.Error(t, err)
}

func TestHTTPTransport_Headers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer token", r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		var req Request
		require.NoError(t, json.Unmarshal(body, &req))
		writeResponse(t, w, req.ID, InitializeResult{ProtocolVersion: "2024-11-05"})
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL, map[string]string{"Authorization": "Bearer token"}, time.Second)
	defer tr.Close()

	_, err := tr.Initialize(context.Background(), InitializeParams{ProtocolVersion: "2024-11-05"})
	require.NoError(t, err)
}

func writeResponse(t *testing.T, w http.ResponseWriter, id int, result any) {
	t.Helper()
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	resp := Response{JSONRPC: jsonRPCVersion, ID: id, Result: raw}
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(resp))
}

func writeError(t *testing.T, w http.ResponseWriter, id, code int, message string) {
	t.Helper()
	resp := Response{JSONRPC: jsonRPCVersion, ID: id, Error: &ErrorObject{Code: code, Message: message}}
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(resp))
}

// TestHTTPTransport_LargeSSEResponse guards against the regression where a
// single SSE `data:` line exceeding the prior 1 MiB bufio.Scanner cap failed
// with `bufio.Scanner: token too long`. The whole JSON-RPC response sits on
// one `data:` line, so a large tool result must be read in full.
func TestHTTPTransport_LargeSSEResponse(t *testing.T) {
	big := strings.Repeat("x", 2<<20) // 2 MiB, well over the old 1 MiB cap
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req Request
		require.NoError(t, json.Unmarshal(body, &req))

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		raw, err := json.Marshal(Response{
			JSONRPC: jsonRPCVersion,
			ID:      req.ID,
			Result:  mustMarshal(ToolsCallResult{Content: []Content{{Type: "text", Text: big}}}),
		})
		require.NoError(t, err)
		fmt.Fprintf(w, "event:message\ndata:%s\n\n", string(raw))
		flusher.Flush()
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL, nil, 5*time.Second)
	defer tr.Close()

	res, err := tr.CallTool(context.Background(), ToolsCallParams{Name: "add"})
	require.NoError(t, err)
	require.Len(t, res.Content, 1)
	require.Equal(t, big, res.Content[0].Text)
}

// TestReadSSEData_Oversized verifies that a single `data:` line over the
// (reduced) limit yields a clear errMessageTooLarge instead of the opaque
// scanner error or silent truncation.
func TestReadSSEData_Oversized(t *testing.T) {
	payload := "data:" + strings.Repeat("x", 200) + "\n"
	_, err := readSSEDataLimited(strings.NewReader(payload), 64)
	require.Error(t, err)
	require.ErrorIs(t, err, errMessageTooLarge)
	require.NotContains(t, err.Error(), "token too long")
}

// TestReadSSEData_LargeUnderLimit verifies a line under the limit is returned
// in full.
func TestReadSSEData_LargeUnderLimit(t *testing.T) {
	big := strings.Repeat("y", 2000)
	payload := "event:message\ndata:" + big + "\n\n"
	out, err := readSSEDataLimited(strings.NewReader(payload), 4096)
	require.NoError(t, err)
	require.Equal(t, big, string(out))
}
