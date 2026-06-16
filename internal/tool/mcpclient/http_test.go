package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
