package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lush/blowball/internal/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRequestBudget(t *testing.T) {
	tests := []struct {
		name       string
		window     int
		completion int
		want       int
		valid      bool
	}{
		{name: "positive", window: 200000, completion: 8192, want: 200000 - 8192 - contextSafetyMarginTokens, valid: true},
		{name: "disabled", window: 0, completion: 8192, want: 0, valid: false},
		{name: "negative remaining", window: 1000, completion: 2000, want: 1000 - 2000 - contextSafetyMarginTokens, valid: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := NewRequestBudget(tc.window, tc.completion)
			assert.Equal(t, tc.want, b.InputTokens)
			assert.Equal(t, tc.valid, b.Valid())
		})
	}
}

func TestEstimateJSONContent_isConservativeForDenseJSON(t *testing.T) {
	// Dense JSON and ids are deliberately charged at three characters per
	// token, not four. This guards against silent underestimation between a
	// prose-heavy estimate and a tool payload sent to the same model.
	const payload = `{"query":"SELECT id,name FROM users WHERE status='active'","ids":["0123456789abcdef","fedcba9876543210"]}`
	got := estimateJSONContent(payload)
	generic := (len([]rune(payload)) + 3) / 4
	assert.Greater(t, got, generic)
}

func TestEstimateRenderedRequest_countsToolsAndImages(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{{ID: "call-1", Function: ToolCallFunction{Name: "bash", Arguments: `{"command":"ls -la"}`}}}},
		{Role: "tool", ToolCallID: "call-1", Name: "bash", Content: `{"status":0,"result":{"output":"file1\nfile2"}}`},
		{Role: "user", Images: []MessageImage{{Data: make([]byte, 4096)}}},
	}
	withTools := EstimateRenderedRequest("system", msgs, []byte(`[{"type":"function","function":{"name":"bash","description":"run"}}]`))
	withoutTools := EstimateRenderedRequest("system", msgs, nil)
	require.Greater(t, withTools.Tokens, withoutTools.Tokens)
	assert.False(t, withTools.MeasuredUsage)
}

func TestEstimateRequestFromUsage_includesNewToolResult(t *testing.T) {
	base := EstimateRequestFromUsage(100, 10, nil)
	withResult := EstimateRequestFromUsage(100, 10, []Message{{Role: "tool", Content: strings.Repeat("x", 4000)}})
	require.True(t, withResult.MeasuredUsage)
	assert.Greater(t, withResult.Tokens, base.Tokens+900)
}

func TestContextBudgetErrors_carryActionableValues(t *testing.T) {
	u := &UncompactableContextError{EstimatedTokens: 900, InputBudget: 800, Reason: "minimal context"}
	assert.Contains(t, u.Error(), "900")
	assert.Contains(t, u.Error(), "800")
	assert.Contains(t, u.Error(), "minimal context")

	r := &RequestTooLargeError{EstimatedTokens: 900, InputBudget: 800}
	var target *RequestTooLargeError
	assert.True(t, errors.As(r, &target))
	assert.Contains(t, r.Error(), "900")
}

func TestSanitizeBudgetReason_boundsAndTrims(t *testing.T) {
	assert.Equal(t, "reason", sanitizeBudgetReason(" reason \n"))
	assert.LessOrEqual(t, len(sanitizeBudgetReason(strings.Repeat("x", 300))), 240)
}

func TestConfucius_InvalidBudgetRejectedBeforeModelCall(t *testing.T) {
	client := newFake()
	c := newTestConfucius(t, client, nil)
	c.turn.MaxContextTokens = 1000
	c.turn.MaxCompletionTokens = 500
	hub := stream.NewHub(stream.DefaultHubBufferSize)

	_, _, _, err := c.Run(context.Background(), []Message{{Role: "user", Content: "hi"}}, hub)
	var invalid *InvalidRequestBudgetError
	require.ErrorAs(t, err, &invalid)
	assert.Equal(t, 0, client.requestCount())
}

func TestConfucius_OversizedFirstRequestCompactedBeforeModelCall(t *testing.T) {
	client := newFake(fakeResponse{content: "recovered", finishReason: "stop"})
	c := newTestConfucius(t, client, nil)
	c.turn.MaxContextTokens = 8000
	c.turn.MaxCompletionTokens = 100
	compactions := 0
	c.SetRoundHook(func(context.Context, Usage) []Message {
		compactions++
		return []Message{{Role: "user", Content: "compacted"}}
	})
	hub := stream.NewHub(stream.DefaultHubBufferSize)

	content, _, _, err := c.Run(context.Background(), []Message{{Role: "user", Content: strings.Repeat("x", 30000)}}, hub)
	require.NoError(t, err)
	assert.Equal(t, "recovered", content)
	assert.Equal(t, 1, compactions)
	require.Equal(t, 1, client.requestCount())
	assert.Contains(t, client.calls[0].Messages[len(client.calls[0].Messages)-1].Content, "compacted")
}

func TestConfucius_UndecompactableFirstRequestRejectedBeforeModelCall(t *testing.T) {
	client := newFake()
	c := newTestConfucius(t, client, nil)
	c.turn.MaxContextTokens = 8000
	c.turn.MaxCompletionTokens = 100
	c.SetRoundHook(func(context.Context, Usage) []Message { return nil })
	hub := stream.NewHub(stream.DefaultHubBufferSize)

	_, _, _, err := c.Run(context.Background(), []Message{{Role: "user", Content: strings.Repeat("x", 30000)}}, hub)
	var tooLarge *RequestTooLargeError
	require.ErrorAs(t, err, &tooLarge)
	assert.Equal(t, 0, client.requestCount(), "an over-budget first request must never reach the provider")
}
