package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
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
	c.maxRounds = 1
	c.turn.MaxContextTokens = 8000
	c.turn.MaxCompletionTokens = 100
	compactions := 0
	c.SetRoundHook(func(ctx context.Context, _ Usage) []Message {
		if _, forced := ForcedCompactionFromContext(ctx); !forced {
			return nil
		}
		compactions++
		return []Message{{Role: "user", Content: "compacted"}}
	})
	hub := stream.NewHub(stream.DefaultHubBufferSize)

	content, _, _, err := c.Run(context.Background(), []Message{{Role: "user", Content: strings.Repeat("x", 30000)}}, hub)
	require.NoError(t, err)
	assert.Equal(t, "recovered", content)
	assert.Equal(t, 1, compactions)
	require.Equal(t, 1, client.requestCount())
	foundSummary := false
	for _, msg := range client.calls[0].Messages {
		if strings.Contains(msg.Content, "compacted") {
			foundSummary = true
			break
		}
	}
	assert.True(t, foundSummary)
}

func TestConfucius_PreflightCompactionPreservesLiveCurrentMessage(t *testing.T) {
	client := newFake(fakeResponse{content: "recovered", finishReason: "stop"})
	c := newTestConfucius(t, client, nil)
	c.maxRounds = 1
	c.turn.MaxContextTokens = 8000
	c.turn.MaxCompletionTokens = 100
	c.SetRoundHook(func(context.Context, Usage) []Message {
		return []Message{{Role: "user", Content: "checkpoint summary"}}
	})
	hub := stream.NewHub(stream.DefaultHubBufferSize)
	live := Message{
		Role:    "user",
		Content: strings.Repeat("x", 30000),
		Images: []MessageImage{{
			AttachmentID: "att-live",
			MIMEType:     "image/png",
			Data:         []byte{1, 2, 3},
		}},
	}

	_, _, _, err := c.Run(context.Background(), []Message{live}, hub)
	require.NoError(t, err)
	require.Equal(t, 1, client.requestCount())
	found := false
	for _, msg := range client.calls[0].Messages {
		if len(msg.Images) == 1 && msg.Images[0].AttachmentID == "att-live" {
			found = true
			assert.Less(t, len([]rune(msg.Content)), 2100, "compacted live-message text must stay bounded")
		}
	}
	assert.True(t, found, "preflight compaction must retain live inline images for the current turn")
}

func TestRestoreLiveMessages_UsesUniqueAttachmentPlaceholders(t *testing.T) {
	live := Message{
		Role:    "user",
		Content: "same question",
		Images:  []MessageImage{{AttachmentID: "att-current", Filename: "current.png", MIMEType: "image/png", Data: []byte{1}}},
	}
	compacted := []Message{
		{Role: "user", Content: "same question\n[图片: old.png]"},
		{Role: "user", Content: "same question\n[图片: current.png]"},
		{Role: "assistant", Content: "answer"},
	}
	got := restoreLiveMessages([]Message{live, {Role: "assistant", Content: "answer"}}, compacted)
	require.Len(t, got, 3)
	assert.Empty(t, got[0].Images, "the unrelated same-content message must not be overwritten")
	require.Len(t, got[1].Images, 1)
	assert.Equal(t, "att-current", got[1].Images[0].AttachmentID)
}

func TestRestoreLiveMessages_UnknownAttachmentDoesNotOverwriteSameContent(t *testing.T) {
	live := Message{
		Role:    "user",
		Content: "same question",
		Images:  []MessageImage{{AttachmentID: "att-new", Filename: "new.png", MIMEType: "image/png", Data: []byte{1}}},
	}
	compacted := []Message{
		{Role: "user", Content: "same question\n[图片: old.png]"},
		{Role: "assistant", Content: "answer"},
	}
	got := restoreLiveMessages([]Message{live, {Role: "assistant", Content: "next"}}, compacted)
	require.Len(t, got, 3)
	assert.Empty(t, got[0].Images)
	require.Len(t, got[2].Images, 1, "an unmatched live identity must be inserted, not mapped to a different message")
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

func TestConfucius_WrapUpIsBudgetCheckedAndCompacted(t *testing.T) {
	client := newFake(
		fakeResponse{
			content:      "calling",
			finishReason: "tool_calls",
			toolCalls: []ToolCall{{
				ID:       "large-1",
				Function: ToolCallFunction{Name: "large_tool", Arguments: `{}`},
			}},
		},
		fakeResponse{content: "final", finishReason: "stop"},
	)
	c := newTestConfucius(t, client, nil)
	c.maxRounds = 1
	c.turn.MaxContextTokens = 8000
	c.turn.MaxCompletionTokens = 100
	if err := c.toolRegistry.Register(&tool.ToolSpec{
		Name:           "large_tool",
		Description:    "returns a large payload",
		ParametersJSON: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(context.Context, json.RawMessage) (any, error) {
			return strings.Repeat("x", 30000), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	compactions := 0
	c.SetRoundHook(func(context.Context, Usage) []Message {
		compactions++
		return []Message{{Role: "user", Content: "compacted"}}
	})
	hub := stream.NewHub(stream.DefaultHubBufferSize)

	content, _, _, err := c.Run(context.Background(), []Message{{Role: "user", Content: "hi"}}, hub)
	require.NoError(t, err)
	assert.Equal(t, "final", content)
	assert.Equal(t, 1, compactions, "wrap-up must be budget checked before its model call")
	require.Equal(t, 2, client.requestCount())
	wrap := client.calls[1]
	assert.LessOrEqual(t, EstimateRenderedRequest("", wrap.Messages, nil).Tokens, 8000-100-contextSafetyMarginTokens)
}

func TestConfucius_WrapUpProviderOverflowRecoversOnce(t *testing.T) {
	client := newFake(
		fakeResponse{
			content:      "calling",
			finishReason: "tool_calls",
			toolCalls: []ToolCall{{
				ID:       "small-1",
				Function: ToolCallFunction{Name: "small_tool", Arguments: `{}`},
			}},
		},
		fakeResponse{err: ErrContextOverflow},
		fakeResponse{content: "recovered wrap-up", finishReason: "stop"},
	)
	c := newTestConfucius(t, client, nil)
	c.maxRounds = 1
	c.turn.MaxContextTokens = 8000
	c.turn.MaxCompletionTokens = 100
	if err := c.toolRegistry.Register(&tool.ToolSpec{
		Name:           "small_tool",
		Description:    "returns a small payload",
		ParametersJSON: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(context.Context, json.RawMessage) (any, error) {
			return "ok", nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	recoveries := 0
	c.SetRoundHook(func(ctx context.Context, _ Usage) []Message {
		force, forced := ForcedCompactionFromContext(ctx)
		if !forced || force.Reason != "wrap_up_overflow" {
			return nil
		}
		recoveries++
		return []Message{{Role: "user", Content: "compacted wrap-up"}}
	})
	hub := stream.NewHub(stream.DefaultHubBufferSize)

	content, _, _, err := c.Run(context.Background(), []Message{{Role: "user", Content: "hi"}}, hub)
	require.NoError(t, err)
	assert.Equal(t, "recovered wrap-up", content)
	assert.Equal(t, 1, recoveries)
	assert.Equal(t, 3, client.requestCount(), "one overflow must lead to one rebuilt wrap-up request")
}
