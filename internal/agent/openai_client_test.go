package agent

import (
	"context"
	"encoding/json"
	"fmt"
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
	))

	var tokens []string
	resp, err := client.StreamChat(context.Background(), LLMRequest{
		Model:           "o3-mini",
		Messages:        []Message{{Role: "user", Content: "hi"}},
		Thinking:        true,
		ReasoningEffort: "medium",
	}, func(tok string) error {
		tokens = append(tokens, tok)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, "Hello", resp.Content)
	assert.Equal(t, "Analyzing the problem", resp.ReasoningContent)
	assert.Equal(t, []string{"Hello"}, tokens)
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
				"role":               "assistant",
				"content":            "Hi",
				"reasoning_content":  nil,
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
	))

	resp, err := client.StreamChat(context.Background(), LLMRequest{
		Model:           "o3-mini",
		Messages:        []Message{{Role: "user", Content: "hi"}},
		Thinking:        true,
		ReasoningEffort: "low",
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "Hi", resp.Content)
	assert.Empty(t, resp.ReasoningContent)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
