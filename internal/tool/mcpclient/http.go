package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// sessionHeader is the MCP Streamable HTTP session id header.
const sessionHeader = "Mcp-Session-Id"

// acceptHeader is sent on every POST so servers that can return either JSON or
// SSE know the client supports both.
const acceptHeader = "application/json, text/event-stream"

// contentTypeSSE is the MIME type returned by Streamable HTTP servers that
// choose to stream responses as SSE events.
const contentTypeSSE = "text/event-stream"

// HTTPTransport implements the MCP Streamable HTTP transport.
// It caches the session id returned by initialize and re-initializes once when
// the server reports a session expiration error.
type HTTPTransport struct {
	baseURL    string
	headers    map[string]string
	httpClient *http.Client

	mu        sync.RWMutex
	sessionID string
	closed    bool

	nextID atomic.Int32
}

// NewHTTPTransport creates an HTTP transport. headers is optional.
func NewHTTPTransport(baseURL string, headers map[string]string, timeout time.Duration) *HTTPTransport {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &HTTPTransport{
		baseURL: strings.TrimRight(baseURL, "/"),
		headers: headers,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// Initialize performs the MCP handshake over HTTP POST and caches the
// Mcp-Session-Id header when the server returns one.
func (t *HTTPTransport) Initialize(ctx context.Context, params InitializeParams) (*InitializeResult, error) {
	resp, err := t.post(ctx, "initialize", params, false)
	if err != nil {
		return nil, fmt.Errorf("http initialize: %w", err)
	}
	var result InitializeResult
	if err := extractResult(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListTools calls tools/list on the remote server.
func (t *HTTPTransport) ListTools(ctx context.Context) (*ToolsListResult, error) {
	resp, err := t.postWithRetry(ctx, "tools/list", struct{}{})
	if err != nil {
		return nil, fmt.Errorf("http tools/list: %w", err)
	}
	var result ToolsListResult
	if err := extractResult(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CallTool calls tools/call on the remote server.
func (t *HTTPTransport) CallTool(ctx context.Context, params ToolsCallParams) (*ToolsCallResult, error) {
	resp, err := t.postWithRetry(ctx, "tools/call", params)
	if err != nil {
		return nil, fmt.Errorf("http tools/call: %w", err)
	}
	var result ToolsCallResult
	if err := extractResult(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Close releases the HTTP transport.
func (t *HTTPTransport) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	if t.httpClient != nil {
		t.httpClient.CloseIdleConnections()
	}
	return nil
}

func (t *HTTPTransport) allocID() int {
	return int(t.nextID.Add(1))
}

func (t *HTTPTransport) session() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.sessionID
}

func (t *HTTPTransport) setSession(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessionID = id
}

func (t *HTTPTransport) isClosed() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.closed
}

// postWithRetry sends a JSON-RPC POST and re-initializes once if the server
// reports a session expiration error.
func (t *HTTPTransport) postWithRetry(ctx context.Context, method string, params any) (*Response, error) {
	resp, err := t.post(ctx, method, params, true)
	if err != nil {
		return nil, err
	}
	if resp.Error == nil || !isSessionExpiredError(resp.Error) {
		return resp, nil
	}

	// Re-initialize and retry once.
	if _, initErr := t.Initialize(ctx, InitializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities:    map[string]any{},
		ClientInfo:      ClientInfo{Name: "blowball", Version: "0.1.0"},
	}); initErr != nil {
		return nil, fmt.Errorf("session expired, re-initialize failed: %w", initErr)
	}
	return t.post(ctx, method, params, true)
}

// post sends a JSON-RPC request over HTTP POST. attachSession controls whether
// the cached Mcp-Session-Id header should be added. It is false for initialize
// so we do not attach a stale session id during re-initialization.
func (t *HTTPTransport) post(ctx context.Context, method string, params any, attachSession bool) (*Response, error) {
	if t.isClosed() {
		return nil, errors.New("http transport closed")
	}

	reqBody, err := newRequest(t.allocID(), method, params)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", acceptHeader)
	for k, v := range t.headers {
		httpReq.Header.Set(k, v)
	}
	if attachSession {
		if sid := t.session(); sid != "" {
			httpReq.Header.Set(sessionHeader, sid)
		}
	}

	httpResp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	// Capture a new session id from any successful response. The spec returns it
	// on initialize, but a server may refresh it on any request.
	if sid := httpResp.Header.Get(sessionHeader); sid != "" {
		t.setSession(sid)
	}

	respBody, err := t.readResponse(httpResp)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode != http.StatusOK && httpResp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("unexpected status %d: %s", httpResp.StatusCode, string(respBody))
	}

	var resp Response
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return &resp, nil
}

// readResponse reads the HTTP response body. For SSE responses it extracts the
// first `data:` line; for JSON responses it returns the body as-is.
func (t *HTTPTransport) readResponse(httpResp *http.Response) ([]byte, error) {
	if strings.Contains(httpResp.Header.Get("Content-Type"), contentTypeSSE) {
		return readSSEData(httpResp.Body)
	}
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	return body, nil
}

// readSSEData scans an SSE stream and returns the payload of the first
// `event:message` data line. It ignores the SSE `id:` and `event:` fields.
func readSSEData(r io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if _, data, ok := strings.Cut(line, "data:"); ok {
			return []byte(strings.TrimSpace(data)), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read sse data: %w", err)
	}
	return nil, fmt.Errorf("no sse data line found")
}

// isSessionExpiredError detects JSON-RPC session expiration errors. The MCP
// spec does not mandate a single error code, so we recognize both common codes
// and substring patterns in the message.
func isSessionExpiredError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "session") && (strings.Contains(text, "expir") || strings.Contains(text, "invalid")) {
		return true
	}
	// JSON-RPC error object surfaces through ErrorObject.Error().
	if strings.Contains(text, "-32001") {
		return true
	}
	return false
}
