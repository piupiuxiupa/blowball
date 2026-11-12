package turn

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
)

func TestRuntimeStatusReplay_OpenAIWireToolTailBoundary(t *testing.T) {
	rows, calls, canonical := toolTailRecallFixture()
	beforeRows, err := json.Marshal(rows)
	require.NoError(t, err)
	messages, lastRow, err := MessagesToAgentMessagesIndexed(rows, calls)
	require.NoError(t, err)
	statuses := []model.RuntimeStatus{
		{StatusID: "current-a", Kind: agent.RuntimeStatusCurrent, Content: "<current_status>A</current_status>", Seq: 1, BeforeClientMsgID: "turn-a:0"},
		{StatusID: "budget-a", Kind: agent.RuntimeStatusToolBudget, Content: "<tool_budget>one</tool_budget>", Seq: 2, AfterToolCallID: "call-a"},
		{StatusID: "current-b", Kind: agent.RuntimeStatusCurrent, Content: "<current_status>B</current_status>", Seq: 3, BeforeClientMsgID: "turn-b:0"},
	}
	replayed, ok := replayRuntimeStatus(messages, lastRow, rows, statuses)
	require.True(t, ok)
	requireCompleteToolGroups(t, replayed)

	requests := make(chan []json.RawMessage, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case requests <- request.Messages:
		case <-r.Context().Done():
			return
		}
		decoded := make([]agent.Message, len(request.Messages))
		for i, raw := range request.Messages {
			if err := json.Unmarshal(raw, &decoded[i]); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		for i, message := range decoded {
			for j, call := range message.ToolCalls {
				result := i + j + 1
				if result >= len(decoded) || decoded[result].Role != "tool" || decoded[result].ToolCallID != call.ID {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error":{"message":"invalid tool message order","type":"invalid_request_error"}}`)
					return
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"test","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}

data: {"id":"test","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`)
	}))
	defer server.Close()
	client := agent.NewOpenAIClient(config.OpenAIConfig{APIKey: "test-key", BaseURL: server.URL + "/v1"})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	broken := []agent.Message{
		replayed[0], canonical[0],
		{Role: "assistant", ToolCalls: []agent.ToolCall{canonical[1].ToolCalls[0], canonical[3].ToolCalls[0]}},
		canonical[2], replayed[4], canonical[4], replayed[7], canonical[5],
	}
	_, err = client.StreamChat(ctx, agent.LLMRequest{
		Model: "test", MaxCompletionTokens: 32, Messages: broken,
	}, nil, nil)
	require.ErrorContains(t, err, "400", "the local upstream must reject the original split tool block")

	for _, payload := range [][]agent.Message{replayed[:5], replayed} {
		var tokens strings.Builder
		response, err := client.StreamChat(ctx, agent.LLMRequest{
			Model: "test", MaxCompletionTokens: 32, Messages: payload,
		}, func(token string) error {
			tokens.WriteString(token)
			return nil
		}, nil)
		require.NoError(t, err)
		assert.Equal(t, "done", response.Content)
		assert.Equal(t, "done", tokens.String(), "runtime status must not become a response token")
	}

	require.Len(t, requests, 3)
	<-requests // the intentionally broken request
	previous := <-requests
	full := <-requests
	require.Len(t, full, 9)
	assert.Equal(t, previous, full[:len(previous)], "the SDK wire messages must retain the complete previous prefix")
	decoded := make([]agent.Message, len(full))
	for i, raw := range full {
		require.NoError(t, json.Unmarshal(raw, &decoded[i]))
	}
	requireCompleteToolGroups(t, decoded)
	assert.Len(t, decoded[2].ToolCalls, 1)
	assert.Len(t, decoded[5].ToolCalls, 1)
	assert.Equal(t, `{ "q": 1 }`, decoded[2].ToolCalls[0].Function.Arguments)
	assert.Equal(t, "call-a", decoded[3].ToolCallID)
	assert.Equal(t, "recall-b", decoded[6].ToolCallID)
	assert.Equal(t, "<tool_budget>one</tool_budget>", decoded[4].Content)
	assert.Equal(t, "<current_status>B</current_status>", decoded[7].Content)
	assert.Equal(t, "next question", decoded[8].Content)
	afterRows, err := json.Marshal(rows)
	require.NoError(t, err)
	assert.Equal(t, string(beforeRows), string(afterRows), "active request serialization must not alter durable rows")
}
