package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

// TestParallelAgent_ConfuseDispatchesBothSubAgents drives the full
// Confuse round-1 parallel dispatch path: a single LLM response with both
// invoke_chongzhi and invoke_liang tool_calls fires both sub-agents
// concurrently. The test asserts the SSE stream carries lifecycle + token
// events from BOTH sub-agents, the final done event aggregates usage, and the
// persisted assistant message is the round-2 summary.
//
// Sub-agent LLM calls share the same scripted client; responses are popped in
// FIFO order so we pre-queue sub-agent responses ahead of the round-2
// summary. Because parallel dispatch lands on a single client mutex, the
// exact ordering between the two sub-agents is non-deterministic — the test
// asserts both contributed, not the interleaving.
//
// Components on the critical path (in addition to those in message_flow_test):
//   - agent.Confuse dispatchToolCalls (errgroup parallel path)
//   - agent.Chongzhi (isolated context + Xizhi tool registry bound to workspace)
//   - agent.Liang (isolated context, no tools)
//   - per-request agent factory rebuilds Chongzhi with the requesting user's
//     workspace root so the Xizhi tools are properly scoped
func TestParallelAgent_ConfuseDispatchesBothSubAgents(t *testing.T) {
	// Track which agent each call originated from so we can verify both fired
	// even though we cannot predict their order against the shared mutex.
	var (
		mu             sync.Mutex
		chongzhiRounds int
		liangRounds    int
		chongzhiTokens []string
		liangTokens    []string
	)

	// Build a wrapped client that counts sub-agent rounds. The underlying
	// scripted client owns the response queue.
	scripted := newScriptedLLMClient()

	tracking := &trackingLLMClient{
		inner: scripted,
		onStream: func(req agent.LLMRequest) {
			// Identify the agent by the system prompt we configured.
			if len(req.Messages) == 0 {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			sys := req.Messages[0].Content
			switch {
			case strings.Contains(sys, "chongzhi"):
				chongzhiRounds++
			case strings.Contains(sys, "liang"):
				liangRounds++
			}
		},
		onToken: func(req agent.LLMRequest, tok string) {
			if len(req.Messages) == 0 {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			sys := req.Messages[0].Content
			switch {
			case strings.Contains(sys, "chongzhi"):
				chongzhiTokens = append(chongzhiTokens, tok)
			case strings.Contains(sys, "liang"):
				liangTokens = append(liangTokens, tok)
			}
		},
	}

	// Queue:
	//  1. Confuse round 1: emit both invoke_* tool calls.
	scripted.responses = append(scripted.responses, scriptedLLMResponse{
		finishReason: "tool_calls",
		toolCalls: []agent.ToolCall{
			{
				ID: "call_c",
				Function: agent.ToolCallFunction{
					Name:      agent.ToolInvokeChongzhi,
					Arguments: `{"task":"write hello","context":"greeting file"}`,
				},
			},
			{
				ID: "call_l",
				Function: agent.ToolCallFunction{
					Name:      agent.ToolInvokeLiang,
					Arguments: `{"task":"analyze greeting"}`,
				},
			},
		},
		usage: agent.Usage{PromptTokens: 30, CompletionTokens: 2, TotalTokens: 32},
	})

	// 2. One sub-agent round (Chongzhi OR Liang, whichever grabs the mutex
	//    first). Both sub-agents consume identical-shaped responses so the
	//    parallel race between them does not matter to the assertions; we
	//    only verify each contributed tokens.
	scripted.responses = append(scripted.responses, scriptedLLMResponse{
		tokens:       []string{"sub-agent-1"},
		content:      "sub-agent-1 done",
		finishReason: "stop",
		usage:        agent.Usage{PromptTokens: 12, CompletionTokens: 1, TotalTokens: 13},
	})
	// 3. The other sub-agent round.
	scripted.responses = append(scripted.responses, scriptedLLMResponse{
		tokens:       []string{"sub-agent-2"},
		content:      "sub-agent-2 done",
		finishReason: "stop",
		usage:        agent.Usage{PromptTokens: 8, CompletionTokens: 1, TotalTokens: 9},
	})

	// 4. Confuse round 2: emit the final summary, finish_reason=stop. The
	//    orchestrator's per-request agent loop consumes this immediately
	//    after the two sub-agent rounds return. The persisted assistant
	//    content is built from the STREAMED TOKENS (the orchestrator
	//    adapter accumulates Confuse token deltas), so the streamed tokens
	//    and the persisted content match.
	summaryTokens := []string{"I dispatched ", "both sub-agents."}
	scripted.responses = append(scripted.responses, scriptedLLMResponse{
		tokens:       summaryTokens,
		content:      strings.Join(summaryTokens, ""),
		finishReason: "stop",
		usage:        agent.Usage{PromptTokens: 50, CompletionTokens: 4, TotalTokens: 54},
	})
	const summary = "I dispatched both sub-agents."

	// 5. TitleService fires asynchronously AFTER the orchestrator finishes
	//    (the handler kicks it off once the assistant message is saved). It
	//    must be queued last so it never steals a sub-agent slot.
	scripted.responses = append(scripted.responses, scriptedLLMResponse{
		content:      "Parallel",
		finishReason: "stop",
		usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	})

	env := newTestEnv(t, tracking)

	token := authToken(t, defaultUserID)
	w := env.postMessage(`{"content":"parallel dispatch please"}`, token)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, "text/event-stream", w.Result().Header.Get("Content-Type"))

	types, payloads := parseSSEBody(t, w.Body.String())
	require.NotEmpty(t, types)

	// Both sub-agents must have produced agent_start and agent_end events.
	var chongzhiStart, chongzhiEnd, liangStart, liangEnd bool
	chongzhiTokenCount := 0
	liangTokenCount := 0
	for i, ty := range types {
		p := payloads[i]
		switch {
		case ty == stream.EventAgentStart && p["agent"] == stream.AgentChongzhi:
			chongzhiStart = true
		case ty == stream.EventAgentEnd && p["agent"] == stream.AgentChongzhi:
			chongzhiEnd = true
		case ty == stream.EventAgentStart && p["agent"] == stream.AgentLiang:
			liangStart = true
		case ty == stream.EventAgentEnd && p["agent"] == stream.AgentLiang:
			liangEnd = true
		case ty == stream.EventToken && p["agent"] == stream.AgentChongzhi:
			chongzhiTokenCount++
		case ty == stream.EventToken && p["agent"] == stream.AgentLiang:
			liangTokenCount++
		}
	}
	assert.True(t, chongzhiStart, "Chongzhi agent_start missing; events=%v", types)
	assert.True(t, chongzhiEnd, "Chongzhi agent_end missing")
	assert.True(t, liangStart, "Liang agent_start missing")
	assert.True(t, liangEnd, "Liang agent_end missing")
	assert.Greater(t, chongzhiTokenCount, 0, "Chongzhi must contribute tokens")
	assert.Greater(t, liangTokenCount, 0, "Liang must contribute tokens")

	// The stream must terminate with a done event carrying aggregated usage.
	requireEventPresent(t, types, stream.EventDone)
	var donePayload map[string]any
	for _, p := range payloads {
		if p["type"] == stream.EventDone {
			donePayload = p
		}
	}
	require.NotNil(t, donePayload)
	usageRaw := donePayload["meta"].(map[string]any)["usage"]
	usage := usageRaw.(map[string]any)
	totalTokens := int64(usage["total_tokens"].(float64))
	assert.Greater(t, totalTokens, int64(0), "aggregated total_tokens must be > 0")

	// The tracking client should have observed exactly one round per
	// sub-agent.
	mu.Lock()
	assert.Equal(t, 1, chongzhiRounds, "Chongzhi must run exactly once")
	assert.Equal(t, 1, liangRounds, "Liang must run exactly once")
	assert.NotEmpty(t, chongzhiTokens)
	assert.NotEmpty(t, liangTokens)
	mu.Unlock()

	// With event-stream storage, every assistant event is persisted. The
	// assistant summary is reconstructed from Confuse round-2 token events.
	require.Eventually(t, func() bool {
		msgs := env.mysqlFake.messagesFor(defaultSessionID)
		var summaryBuf string
		for _, m := range msgs {
			if m.Agent == stream.AgentConfuse && m.EventType == model.EventTypeToken {
				summaryBuf += m.Content
			}
		}
		return summaryBuf == summary
	}, 2*time.Second, 10*time.Millisecond, "Confuse round-2 token events must reconstruct the summary")

	// Sanity: many rows persisted (user + all assistant events).
	msgs := env.mysqlFake.messagesFor(defaultSessionID)
	require.Greater(t, len(msgs), 2, "expected user message plus multiple assistant events")

	// The user row must be first and correctly tagged.
	require.Equal(t, model.AgentUser, msgs[0].Agent)
	require.Equal(t, model.EventTypeMessage, msgs[0].EventType)
	require.Equal(t, 0, msgs[0].MsgIndex)

	// RecoverMessages returns the full ordered event stream.
	recovered, err := env.msgSvc.RecoverMessages(context.Background(), defaultUserID, defaultSessionID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(recovered), 3)
	assert.Equal(t, model.AgentUser, recovered[0].Agent)
	assert.Equal(t, model.EventTypeMessage, recovered[0].EventType)

	// FS tier carries the same ordered stream.
	sessionFile := filepath.Join(env.dataDir, defaultUserID, "sessions", defaultSessionID+".json")
	data, err := os.ReadFile(sessionFile)
	require.NoError(t, err)
	var doc struct {
		Messages []json.RawMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))
	require.Greater(t, len(doc.Messages), 2)
	var firstFS model.Message
	require.NoError(t, json.Unmarshal(doc.Messages[0], &firstFS))
	assert.Equal(t, model.AgentUser, firstFS.Agent)
	assert.Equal(t, model.EventTypeMessage, firstFS.EventType)
}

// trackingLLMClient wraps an agent.LLMClient, invoking the supplied hooks
// around each StreamChat call so tests can count rounds per agent without
// owning the response queue.
type trackingLLMClient struct {
	inner    agent.LLMClient
	onStream func(agent.LLMRequest)
	onToken  func(agent.LLMRequest, string)
}

func (t *trackingLLMClient) StreamChat(ctx context.Context, req agent.LLMRequest, onToken func(string) error, onReasoning func(string) error) (agent.LLMResponse, error) {
	if t.onStream != nil {
		t.onStream(req)
	}
	wrapped := onToken
	if t.onToken != nil && onToken != nil {
		wrapped = func(delta string) error {
			t.onToken(req, delta)
			return onToken(delta)
		}
	}
	return t.inner.StreamChat(ctx, req, wrapped, onReasoning)
}
