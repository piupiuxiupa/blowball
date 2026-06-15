package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testMCPSSEServer is a minimal MCP server that speaks the SSE transport.
type testMCPSSEServer struct {
	server   *httptest.Server
	endpoint string
	mu       sync.Mutex
	clients  []chan *Response
}

func newTestMCPSSEServer(t *testing.T) *testMCPSSEServer {
	t.Helper()
	ts := &testMCPSSEServer{}

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", ts.handleSSE)
	mux.HandleFunc("/messages", ts.handleMessages)
	ts.server = httptest.NewServer(mux)
	ts.endpoint = "/messages?session=1"
	return ts
}

func (s *testMCPSSEServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", s.endpoint)
	flusher.Flush()

	ch := make(chan *Response, 16)
	s.mu.Lock()
	s.clients = append(s.clients, ch)
	s.mu.Unlock()

	for {
		select {
		case resp := <-ch:
			body, _ := json.Marshal(resp)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *testMCPSSEServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)

	resp := Response{JSONRPC: jsonRPCVersion, ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = mustMarshalJSON(InitializeResult{ProtocolVersion: "2024-11-05"})
	case "tools/list":
		resp.Result = mustMarshalJSON(ToolsListResult{Tools: []Tool{{Name: "add", Description: "adds"}}})
	case "tools/call":
		resp.Result = mustMarshalJSON(ToolsCallResult{Content: []Content{{Type: "text", Text: "done"}}})
	default:
		resp.Error = &ErrorObject{Code: -32601, Message: "method not found"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.clients {
		select {
		case ch <- &resp:
		default:
		}
	}
}

func mustMarshalJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestSSETransport_Initialize(t *testing.T) {
	ts := newTestMCPSSEServer(t)
	defer ts.server.Close()

	tr := NewSSETransport(ts.server.URL, nil, 5*time.Second)
	defer tr.Close()

	res, err := tr.Initialize(context.Background(), InitializeParams{ProtocolVersion: "2024-11-05"})
	require.NoError(t, err)
	require.Equal(t, "2024-11-05", res.ProtocolVersion)
}

func TestSSETransport_ListTools(t *testing.T) {
	ts := newTestMCPSSEServer(t)
	defer ts.server.Close()

	tr := NewSSETransport(ts.server.URL, nil, 5*time.Second)
	defer tr.Close()

	_, err := tr.Initialize(context.Background(), InitializeParams{ProtocolVersion: "2024-11-05"})
	require.NoError(t, err)

	list, err := tr.ListTools(context.Background())
	require.NoError(t, err)
	require.Len(t, list.Tools, 1)
}

func TestSSETransport_CallTool(t *testing.T) {
	ts := newTestMCPSSEServer(t)
	defer ts.server.Close()

	tr := NewSSETransport(ts.server.URL, nil, 5*time.Second)
	defer tr.Close()

	_, err := tr.Initialize(context.Background(), InitializeParams{ProtocolVersion: "2024-11-05"})
	require.NoError(t, err)

	res, err := tr.CallTool(context.Background(), ToolsCallParams{Name: "add", Arguments: json.RawMessage(`{"a":1}`)})
	require.NoError(t, err)
	require.Len(t, res.Content, 1)
	require.Equal(t, "done", res.Content[0].Text)
}

// silence unused warning for strings import used by server construction.
var _ = strings.Contains
