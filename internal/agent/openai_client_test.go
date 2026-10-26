package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lush/blowball/internal/stream"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenAIClient_StreamChat_CapturesReasoningContent verifies that the client
// extracts reasoning_content deltas from the streaming response and surfaces
// them in the returned LLMResponse and debug log fields.
func TestOpenAIClient_StreamChat_CapturesReasoningContent(t *testing.T) {
	// SSE stream with a reasoning_content delta followed by a content delta.
	body := `data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"o3-mini","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Analyzing"},"finish_reason":null}]}

data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"o3-mini","choices":[{"index":0,"delta":{"reasoning_content":" the problem"},"finish_reason":null}]}

data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"o3-mini","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"o3-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := NewOpenAIClientFromClient(openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(srv.URL+"/v1"),
	), 0)

	var tokens []string
	var reasoningTokens []string
	resp, err := client.StreamChat(context.Background(), LLMRequest{
		Model:           "o3-mini",
		Messages:        []Message{{Role: "user", Content: "hi"}},
		Thinking:        true,
		ReasoningEffort: "medium",
	}, func(tok string) error {
		tokens = append(tokens, tok)
		return nil
	}, func(tok string) error {
		reasoningTokens = append(reasoningTokens, tok)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, "Hello", resp.Content)
	assert.Equal(t, "Analyzing the problem", resp.ReasoningContent)
	assert.Equal(t, []string{"Hello"}, tokens)
	assert.Equal(t, []string{"Analyzing", " the problem"}, reasoningTokens)
}

// TestOpenAIClient_StreamChat_ReasoningContentNullIgnored verifies that a null
// reasoning_content delta does not pollute the accumulated reasoning content.
func TestOpenAIClient_StreamChat_ReasoningContentNullIgnored(t *testing.T) {
	body := fmt.Sprintf(`data: %s

data: [DONE]

`, mustJSON(map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion.chunk",
		"created": 1,
		"model":   "o3-mini",
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{
				"role":              "assistant",
				"content":           "Hi",
				"reasoning_content": nil,
			},
			"finish_reason": nil,
		}},
	}))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := NewOpenAIClientFromClient(openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(srv.URL+"/v1"),
	), 0)

	resp, err := client.StreamChat(context.Background(), LLMRequest{
		Model:           "o3-mini",
		Messages:        []Message{{Role: "user", Content: "hi"}},
		Thinking:        true,
		ReasoningEffort: "low",
	}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "Hi", resp.Content)
	assert.Empty(t, resp.ReasoningContent)
}

// TestOpenAIClient_StreamChat_EmptyReasoningDeltaDropped verifies that gateways
// which stamp an empty reasoning_content on every content chunk do not produce
// empty reasoning deltas: the callback fires only for non-empty fragments, and
// the aggregated reasoning stays unpolluted. Without this guard each empty
// delta becomes an empty reasoning event downstream, which both persists an
// empty row and breaks adjacency-based token merging.
func TestOpenAIClient_StreamChat_EmptyReasoningDeltaDropped(t *testing.T) {
	body := `data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"o3-mini","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Analyzing"},"finish_reason":null}]}

data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"o3-mini","choices":[{"index":0,"delta":{"content":"Hel","reasoning_content":""},"finish_reason":null}]}

data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"o3-mini","choices":[{"index":0,"delta":{"content":"lo","reasoning_content":""},"finish_reason":null}]}

data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"o3-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := NewOpenAIClientFromClient(openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(srv.URL+"/v1"),
	), 0)

	var tokens []string
	var reasoningTokens []string
	resp, err := client.StreamChat(context.Background(), LLMRequest{
		Model:           "o3-mini",
		Messages:        []Message{{Role: "user", Content: "hi"}},
		Thinking:        true,
		ReasoningEffort: "medium",
	}, func(tok string) error {
		tokens = append(tokens, tok)
		return nil
	}, func(tok string) error {
		reasoningTokens = append(reasoningTokens, tok)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, "Hello", resp.Content)
	assert.Equal(t, "Analyzing", resp.ReasoningContent)
	assert.Equal(t, []string{"Hel", "lo"}, tokens)
	assert.Equal(t, []string{"Analyzing"}, reasoningTokens)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestClassifyOpenAIError_contextOverflow(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{
			name: "gateway input tokens exceed model context",
			msg:  `400 Bad Request: {"error":{"message":"input tokens exceed the model context limit"}}`,
			want: true,
		},
		{
			name: "openai context length code",
			msg:  `Error code: 400 - {"error":{"code":"context_length_exceeded","message":"Please reduce the length of the messages."}}`,
			want: true,
		},
		{
			name: "malformed request is semantic",
			msg:  `400 Bad Request: {"error":{"message":"unknown parameter"}}`,
			want: false,
		},
		{
			name: "rate limit is not overflow",
			msg:  `429 Too Many Requests`,
			want: false,
		},
		{
			name: "server error is not overflow",
			msg:  `503 Service Unavailable`,
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyOpenAIError(errors.New(tc.msg))
			assert.Equal(t, tc.want, errors.Is(err, ErrContextOverflow))
			if !tc.want {
				assert.Contains(t, err.Error(), tc.msg)
			} else {
				assert.Contains(t, err.Error(), tc.msg)
			}
		})
	}
}

func TestIsTransientError_contextOverflowIsNotRetried(t *testing.T) {
	err := classifyOpenAIError(errors.New("input tokens exceed the model context limit"))
	require.ErrorIs(t, err, ErrContextOverflow)
	assert.False(t, isTransientError(err), "context overflow must enter forced compaction, not transient retry")
}

func TestConfucius_ContextOverflowForcesOneCompaction(t *testing.T) {
	ctx := context.Background()
	hub := stream.NewHub(stream.DefaultHubBufferSize)
	client := &fakeLLMClient{responses: []fakeResponse{
		{err: ErrContextOverflow},
		{content: "recovered", finishReason: "stop", usage: Usage{PromptTokens: 20, CompletionTokens: 2}},
	}}
	c := newTestConfucius(t, client, nil)
	c.turn.MaxContextTokens = 8000
	c.turn.MaxCompletionTokens = 100
	calls := 0
	c.SetRoundHook(func(context.Context, Usage) []Message {
		calls++
		return []Message{{Role: "user", Content: "compacted"}}
	})
	content, _, _, err := c.Run(ctx, []Message{{Role: "user", Content: "hi"}}, hub)
	require.NoError(t, err)
	assert.Equal(t, "recovered", content)
	assert.Equal(t, 1, calls, "forced compaction must run exactly once")
}

func TestConfucius_SecondContextOverflowStopsBoundedRecovery(t *testing.T) {
	hub := stream.NewHub(stream.DefaultHubBufferSize)
	client := &fakeLLMClient{responses: []fakeResponse{
		{err: ErrContextOverflow},
		{err: ErrContextOverflow},
	}}
	c := newTestConfucius(t, client, nil)
	c.turn.MaxContextTokens = 8000
	c.turn.MaxCompletionTokens = 100
	calls := 0
	c.SetRoundHook(func(context.Context, Usage) []Message {
		calls++
		return []Message{{Role: "user", Content: "compacted"}}
	})
	_, _, _, err := c.Run(context.Background(), []Message{{Role: "user", Content: "hi"}}, hub)
	require.Error(t, err)
	var uncompactable *UncompactableContextError
	assert.ErrorAs(t, err, &uncompactable)
	assert.Equal(t, 8000-100-contextSafetyMarginTokens, uncompactable.InputBudget)
	assert.Equal(t, 1, calls, "recovery may compact at most once")
	assert.Equal(t, 2, client.requestCount(), "the unchanged request may not be retried again")
}

// sseDone is the minimal terminal SSE payload every request-branch test below
// serves; the tests assert on the captured REQUEST body, not the response.
const sseDone = `data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}

data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

// captureRequestBody runs one StreamChat against a capture server and returns
// the JSON body the client sent.
func captureRequestBody(t *testing.T, req LLMRequest) string {
	t.Helper()
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseDone))
	}))
	defer srv.Close()

	client := NewOpenAIClientFromClient(openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(srv.URL+"/v1"),
	), 0)
	_, err := client.StreamChat(context.Background(), req, nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, gotBody)
	return gotBody
}

// TestOpenAIClient_RequestBranch_NonThinkingFamilyOmitsEffort verifies the
// non-thinking wire family (model-effort-v2 B2): a thinking:false entry sends
// NO reasoning_effort, both quota spellings, and no sampling parameters.
func TestOpenAIClient_RequestBranch_NonThinkingFamilyOmitsEffort(t *testing.T) {
	body := captureRequestBody(t, LLMRequest{
		Model:               "glm-4.7",
		Messages:            []Message{{Role: "user", Content: "hi"}},
		MaxCompletionTokens: 512,
		Thinking:            false,
		// The effort axis is clamped to none before the request is built;
		// the value here is inert on this family.
		ReasoningEffort: "none",
	})
	assert.NotContains(t, body, "reasoning_effort", "non-thinking entries must not send reasoning_effort")
	assert.Contains(t, body, `"max_completion_tokens":512`)
	assert.Contains(t, body, `"max_tokens":512`)
	assert.NotContains(t, body, "temperature", "the always-0 temperature dead parameter is removed")
}

// TestOpenAIClient_RequestBranch_EffortSendsReasoningParams verifies the
// thinking wire family: reasoning_effort rides the request and the token
// budget is sent as max_completion_tokens (the OpenAI reasoning-model
// parameter).
func TestOpenAIClient_RequestBranch_EffortSendsReasoningParams(t *testing.T) {
	body := captureRequestBody(t, LLMRequest{
		Model:               "gpt-5",
		Messages:            []Message{{Role: "user", Content: "hi"}},
		MaxCompletionTokens: 512,
		Thinking:            true,
		ReasoningEffort:     "high",
	})
	assert.Contains(t, body, `"reasoning_effort":"high"`)
	assert.Contains(t, body, `"max_completion_tokens":512`)
	assert.NotContains(t, body, `"max_tokens"`, "reasoning branch must not send max_tokens")
	assert.NotContains(t, body, "temperature", "reasoning branch must not send temperature")
}

// TestOpenAIClient_RequestBranch_NoneEffortSentLiterally verifies the B2 wire
// family's core fix (model-effort-v2): a thinking entry with effort "none"
// sends reasoning_effort as the literal "none" — never omits the parameter —
// so OpenAI-compatible gateways actually disable thinking instead of falling
// back to the model's own default level.
func TestOpenAIClient_RequestBranch_NoneEffortSentLiterally(t *testing.T) {
	body := captureRequestBody(t, LLMRequest{
		Model:               "glm-5.2",
		Messages:            []Message{{Role: "user", Content: "hi"}},
		MaxCompletionTokens: 512,
		Thinking:            true,
		ReasoningEffort:     "none",
	})
	assert.Contains(t, body, `"reasoning_effort":"none"`, "none must be sent as a literal value on thinking entries")
	assert.Contains(t, body, `"max_completion_tokens":512`)
	assert.NotContains(t, body, `"max_tokens"`, "thinking family uses max_completion_tokens even with effort none")
}

// TestOpenAIClient_WireHistoricalUnregisteredToolPair pins the wire shape
// for a transcript carrying a historical tool call whose name is NOT in the
// current tools list (memory-recall-cache-stable: the synthetic
// recall_memories pair). OpenAI-compatible gateways accept assistant
// tool_calls + tool results from history regardless of the current tool
// registry — the same shape every renamed/removed tool already replays
// through reconstruction — and the client must serialize the pair with its
// id, name, arguments, and result intact.
func TestOpenAIClient_WireHistoricalUnregisteredToolPair(t *testing.T) {
	body := `data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}

data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &gotReq))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := NewOpenAIClientFromClient(openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(srv.URL+"/v1"),
	), 0)

	const args = `{"entries":["viking://u/mem/prefs"],"query":"which shell"}`
	resp, err := client.StreamChat(context.Background(), LLMRequest{
		Model: "m",
		Messages: []Message{
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID:       "recall-abc",
				Function: ToolCallFunction{Name: "recall_memories", Arguments: args},
			}}},
			{Role: "tool", ToolCallID: "recall-abc", Name: "recall_memories", Content: "<user-memories>\nfacts\n</user-memories>"},
			{Role: "user", Content: "which shell do I use?"},
		},
		Tools: []byte(`[{"type":"function","function":{"name":"executor_bash","description":"run a command"}}]`),
	}, func(string) error { return nil }, func(string) error { return nil })
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)

	// The pair serializes intact on the wire...
	msgs := gotReq["messages"].([]any)
	require.GreaterOrEqual(t, len(msgs), 3)
	callMsg, _ := msgs[0].(map[string]any)
	toolMsg, _ := msgs[1].(map[string]any)
	userMsg, _ := msgs[len(msgs)-1].(map[string]any)
	assert.Equal(t, "assistant", callMsg["role"])
	calls, _ := callMsg["tool_calls"].([]any)
	require.Len(t, calls, 1)
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	assert.Equal(t, "recall-abc", call["id"])
	assert.Equal(t, "recall_memories", fn["name"])
	assert.JSONEq(t, args, fmt.Sprint(fn["arguments"]))
	assert.Equal(t, "tool", toolMsg["role"])
	assert.Equal(t, "recall-abc", toolMsg["tool_call_id"], "wire pairing is by tool_call_id; the name field is not sent")
	assert.Contains(t, toolMsg["content"], "<user-memories>")
	assert.Equal(t, "user", userMsg["role"])

	// ...while the current tools list does not offer it as callable.
	tools, _ := gotReq["tools"].([]any)
	require.NotEmpty(t, tools)
	for _, tl := range tools {
		tm, _ := tl.(map[string]any)
		f, _ := tm["function"].(map[string]any)
		assert.NotEqual(t, "recall_memories", f["name"],
			"recall_memories is a transcript artifact, never a callable tool")
	}
}
