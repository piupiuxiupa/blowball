package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
)

// mockTransport is a test double implementing Transport.
type mockTransport struct {
	initResult *InitializeResult
	initErr    error
	tools      []Tool
	listErr    error
	callResult *ToolsCallResult
	callErr    error
	closed     bool
}

func (m *mockTransport) Initialize(ctx context.Context, params InitializeParams) (*InitializeResult, error) {
	if m.initErr != nil {
		return nil, m.initErr
	}
	return m.initResult, nil
}

func (m *mockTransport) ListTools(ctx context.Context) (*ToolsListResult, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return &ToolsListResult{Tools: m.tools}, nil
}

func (m *mockTransport) CallTool(ctx context.Context, params ToolsCallParams) (*ToolsCallResult, error) {
	if m.callErr != nil {
		return nil, m.callErr
	}
	if m.callResult != nil {
		return m.callResult, nil
	}
	return &ToolsCallResult{Content: []Content{{Type: "text", Text: "ok"}}}, nil
}

func (m *mockTransport) Close() error {
	m.closed = true
	return nil
}

func mustMarshal(t any) json.RawMessage {
	b, err := json.Marshal(t)
	if err != nil {
		panic(err)
	}
	return b
}

var _ = errors.New
