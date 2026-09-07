package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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
// NO reasoning_effort, plain max_tokens, and no sampling parameters.
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
	assert.Contains(t, body, `"max_tokens":512`)
	assert.NotContains(t, body, "max_completion_tokens", "non-reasoning branch must not send max_completion_tokens")
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
