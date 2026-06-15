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
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SSETransport implements the MCP SSE transport over HTTP.
type SSETransport struct {
	baseURL    string
	headers    map[string]string
	httpClient *http.Client
	endpoint   string

	mu       sync.RWMutex
	eventCh  chan sseEvent
	errCh    chan error
	respCh   chan *Response
	closed   bool
	stopCh   chan struct{}
	readerWg sync.WaitGroup
	sseBody  io.ReadCloser

	nextID atomic.Int32
}

// NewSSETransport creates an SSE transport. headers is optional.
func NewSSETransport(baseURL string, headers map[string]string, timeout time.Duration) *SSETransport {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &SSETransport{
		baseURL: strings.TrimRight(baseURL, "/"),
		headers: headers,
		httpClient: &http.Client{
			Timeout:   0, // streaming connection is long-lived
			Transport: &http.Transport{ResponseHeaderTimeout: timeout},
		},
		nextID: atomic.Int32{},
	}
}

// Initialize connects to the SSE endpoint and performs the handshake.
func (t *SSETransport) Initialize(ctx context.Context, params InitializeParams) (*InitializeResult, error) {
	if err := t.connect(ctx); err != nil {
		return nil, fmt.Errorf("sse connect: %w", err)
	}
	req, err := newRequest(1, "initialize", params)
	if err != nil {
		return nil, err
	}
	resp, err := t.sendRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("sse initialize: %w", err)
	}
	var result InitializeResult
	if err := extractResult(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListTools calls tools/list on the remote server.
func (t *SSETransport) ListTools(ctx context.Context) (*ToolsListResult, error) {
	req, err := newRequest(t.allocID(), "tools/list", struct{}{})
	if err != nil {
		return nil, err
	}
	resp, err := t.sendRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("sse tools/list: %w", err)
	}
	var result ToolsListResult
	if err := extractResult(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CallTool calls tools/call on the remote server.
func (t *SSETransport) CallTool(ctx context.Context, params ToolsCallParams) (*ToolsCallResult, error) {
	req, err := newRequest(t.allocID(), "tools/call", params)
	if err != nil {
		return nil, err
	}
	resp, err := t.sendRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("sse tools/call: %w", err)
	}
	var result ToolsCallResult
	if err := extractResult(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Close shuts down the SSE reader and releases resources.
func (t *SSETransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.stopCh)
	body := t.sseBody
	t.mu.Unlock()

	if body != nil {
		_ = body.Close()
	}
	if t.httpClient != nil {
		t.httpClient.CloseIdleConnections()
	}
	t.readerWg.Wait()
	return nil
}

func (t *SSETransport) allocID() int {
	return int(t.nextID.Add(1))
}

func (t *SSETransport) connect(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return errors.New("sse transport closed")
	}

	t.eventCh = make(chan sseEvent, 16)
	t.errCh = make(chan error, 1)
	t.respCh = make(chan *Response, 16)
	t.stopCh = make(chan struct{})

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL+"/sse", nil)
	if err != nil {
		return err
	}
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "text/event-stream")

	rawResp, err := t.httpClient.Do(req)
	if err != nil {
		return err
	}
	if rawResp.StatusCode != http.StatusOK {
		_ = rawResp.Body.Close()
		return fmt.Errorf("unexpected status %d", rawResp.StatusCode)
	}

	t.readerWg.Add(1)
	t.sseBody = rawResp.Body
	go t.readLoop(rawResp.Body)

	// Wait for the endpoint event before accepting requests.
	select {
	case evt := <-t.eventCh:
		if evt.event != "endpoint" {
			_ = rawResp.Body.Close()
			return fmt.Errorf("expected endpoint event, got %q", evt.event)
		}
		t.endpoint = resolveEndpoint(t.baseURL, evt.data)
	case err := <-t.errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}

func (t *SSETransport) readLoop(body io.ReadCloser) {
	defer t.readerWg.Done()
	defer body.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 1024*1024)

	var current sseEvent
	for {
		select {
		case <-t.stopCh:
			return
		default:
		}

		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				select {
				case t.errCh <- err:
				default:
				}
			}
			return
		}

		line := scanner.Text()
		if line == "" {
			t.dispatchEvent(current)
			current = sseEvent{}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			current.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			current.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

func (t *SSETransport) dispatchEvent(evt sseEvent) {
	if evt.event == "" {
		return
	}
	switch evt.event {
	case "endpoint":
		select {
		case t.eventCh <- evt:
		default:
		}
	case "message":
		var resp Response
		if err := json.Unmarshal([]byte(evt.data), &resp); err != nil {
			select {
			case t.errCh <- fmt.Errorf("sse unmarshal message: %w", err):
			default:
			}
			return
		}
		select {
		case t.respCh <- &resp:
		default:
		}
	}
}

func (t *SSETransport) sendRequest(ctx context.Context, req *Request) (*Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	if t.endpoint == "" {
		return nil, errors.New("sse endpoint not established")
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range t.headers {
		httpReq.Header.Set(k, v)
	}

	httpResp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK && httpResp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("unexpected POST status %d", httpResp.StatusCode)
	}

	select {
	case resp := <-t.respCh:
		if resp.ID != req.ID {
			return nil, fmt.Errorf("response id mismatch: got %d want %d", resp.ID, req.ID)
		}
		return resp, nil
	case err := <-t.errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type sseEvent struct {
	event string
	data  string
}

func resolveEndpoint(baseURL, endpoint string) string {
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return endpoint
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return baseURL + endpoint
	}
	ref, err := url.Parse(endpoint)
	if err != nil {
		return baseURL + endpoint
	}
	return base.ResolveReference(ref).String()
}
