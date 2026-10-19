package agent

import (
	"context"
	"encoding/json"

	"github.com/lush/blowball/internal/stream"
)

// emitToolCallEvents sends every call placeholder in LLM response order before
// tool execution starts. Keeping this pass synchronous removes goroutine
// scheduling from durable call order and lets the round renderer treat a call
// after a completed result as an unambiguous new LLM round.
func emitToolCallEvents(ctx context.Context, hub stream.EventHub, agentName string, calls []ToolCall) map[string]toolResult {
	failures := make(map[string]toolResult, len(calls))
	for _, tc := range calls {
		if hub.SendCtx(ctx, stream.ToolCallEvent(agentName, tc.ID, tc.Function.Name, json.RawMessage(tc.Function.Arguments))) {
			continue
		}
		failures[tc.ID] = toolResult{content: "", isError: true}
	}
	return failures
}
