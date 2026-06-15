package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
)

// StdioTransport implements the MCP stdio transport by spawning a subprocess and
// speaking newline-delimited JSON-RPC over stdin/stdout.
type StdioTransport struct {
	command string
	args    []string
	env     map[string]string

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	respCh chan *Response
	errCh  chan error
	closed bool
	stopCh chan struct{}
	wg     sync.WaitGroup

	nextID atomic.Int32
}

// NewStdioTransport creates a stdio transport.
func NewStdioTransport(command string, args []string, env map[string]string) *StdioTransport {
	return &StdioTransport{
		command: command,
		args:    append([]string(nil), args...),
		env:     env,
	}
}

// Initialize starts the subprocess and performs the MCP handshake.
func (t *StdioTransport) Initialize(ctx context.Context, params InitializeParams) (*InitializeResult, error) {
	if err := t.start(ctx); err != nil {
		return nil, fmt.Errorf("stdio start: %w", err)
	}
	req, err := newRequest(1, "initialize", params)
	if err != nil {
		return nil, err
	}
	resp, err := t.sendRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("stdio initialize: %w", err)
	}
	var result InitializeResult
	if err := extractResult(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListTools calls tools/list on the remote server.
func (t *StdioTransport) ListTools(ctx context.Context) (*ToolsListResult, error) {
	req, err := newRequest(t.allocID(), "tools/list", struct{}{})
	if err != nil {
		return nil, err
	}
	resp, err := t.sendRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("stdio tools/list: %w", err)
	}
	var result ToolsListResult
	if err := extractResult(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CallTool calls tools/call on the remote server.
func (t *StdioTransport) CallTool(ctx context.Context, params ToolsCallParams) (*ToolsCallResult, error) {
	req, err := newRequest(t.allocID(), "tools/call", params)
	if err != nil {
		return nil, err
	}
	resp, err := t.sendRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("stdio tools/call: %w", err)
	}
	var result ToolsCallResult
	if err := extractResult(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Close terminates the subprocess and releases resources.
func (t *StdioTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.stopCh)
	stdin := t.stdin
	cmd := t.cmd
	t.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	t.wg.Wait()
	return nil
}

func (t *StdioTransport) allocID() int {
	return int(t.nextID.Add(1))
}

func (t *StdioTransport) start(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return errors.New("stdio transport closed")
	}

	t.respCh = make(chan *Response, 16)
	t.errCh = make(chan error, 1)
	t.stopCh = make(chan struct{})

	cmd := exec.CommandContext(ctx, t.command, t.args...)
	cmd.Env = t.buildEnv()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}

	t.cmd = cmd
	t.stdin = stdin
	t.stdout = stdout
	t.stderr = stderr

	t.wg.Add(2)
	go t.readLoop(stdout)
	go t.drainStderr(stderr)

	return nil
}

func (t *StdioTransport) buildEnv() []string {
	base := os.Environ()
	if len(t.env) == 0 {
		return base
	}
	extra := make([]string, 0, len(t.env))
	for k, v := range t.env {
		extra = append(extra, fmt.Sprintf("%s=%s", k, v))
	}
	return append(extra, base...)
}

func (t *StdioTransport) readLoop(r io.Reader) {
	defer t.wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
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

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			select {
			case t.errCh <- fmt.Errorf("stdio unmarshal: %w", err):
			default:
			}
			continue
		}
		select {
		case t.respCh <- &resp:
		default:
		}
	}
}

func (t *StdioTransport) drainStderr(r io.Reader) {
	defer t.wg.Done()
	// Discard stderr to avoid blocking the child process. A production
	// deployment may pipe this to the application logger.
	_, _ = io.Copy(io.Discard, r)
}

func (t *StdioTransport) sendRequest(ctx context.Context, req *Request) (*Response, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, errors.New("stdio transport closed")
	}
	stdin := t.stdin
	t.mu.Unlock()

	if stdin == nil {
		return nil, errors.New("stdio transport not initialized")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')

	if _, err := stdin.Write(body); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
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
