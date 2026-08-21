package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/stream"
	"go.uber.org/zap"
)

// llm-length-continuation capability: when a round's LLM response ends
// finish_reason=length, the agent keeps the partial output and continues with
// an expanded budget instead of silently terminating the turn. This file
// holds the shared round runner used by the three agent loops and the
// round-cap wrap-up round (design D3); the caller still owns loop-level
// semantics (round counting, terminal branches, usage attribution).

// lengthContinueInstruction is appended (user role) to the round when a
// content-only truncation is continued: it steers the model to resume exactly
// where it stopped without repeating itself, and reinforces the
// split-large-writes guidance (the same pattern the write-tool descriptions
// carry; see tools.go injection). It stays in the round for the rest of the
// turn. Like wrapUpInstruction it is round-local scaffolding: it is never
// emitted as an SSE event and never persisted (context-shape drift across
// turns is accepted — content stays equivalent).
const lengthContinueInstruction = "Your previous output was cut off at the output token limit. Continue exactly where the output stopped and complete the response. Do NOT repeat content you have already written. When producing large file writes, do not try to emit the whole content in one output: write the first part, then append the rest with additional, smaller writes."

// truncatedToolCallResult is the synthetic role="tool" body for a tool_call
// whose arguments were cut mid-JSON by the length limit (B-call-1 triage):
// the call is never executed, and the message tells the model why and how to
// recover. The tool_call/tool_result SSE pair mirrors the bad_args visibility
// precedent (the failure is visible in the stream), except the tool_call
// event's arguments are sanitized to `{}` — embedding the invalid JSON would
// corrupt the enclosing event payload.
func truncatedToolCallResult(toolName string) string {
	return fmt.Sprintf("not executed: the arguments of tool %q were truncated by the output length limit, so the call was NOT run. Re-issue the call with complete arguments; split large content into multiple smaller writes instead of one huge output.", toolName)
}

// wrapUpToolCallNotExecutableResult is the synthetic role="tool" body for
// parseable tool_calls emitted during a wrap-up round's continuation: tools
// are stripped in the wrap-up, so the calls cannot be honored at all (the
// normal dispatch path is unavailable — dispatch is nil).
const wrapUpToolCallNotExecutableResult = "not executed: tool calls are disabled in this wrap-up round. Do not re-issue tool calls; write the final answer as plain text."

// lengthRoundDispatch executes the given tool_calls of a length response
// whose arguments parsed as complete JSON. The wire protocol requires every
// assistant tool_call to be answered by a role="tool" message before the next
// request, so the complete calls dispatch HERE — inside the continuation
// loop — and the closure must append each result message to the round (the
// agents implement it over their extracted dispatch-and-record block). Nil
// for tool-less rounds (wrap-up), where parseable calls get the synthetic
// wrap-up result instead.
type lengthRoundDispatch func(ctx context.Context, calls []ToolCall)

// roundResult is what one logical round produced across ALL of its attempts.
type roundResult struct {
	// Resp is the FINAL attempt's response (stop-family, or the last
	// length response when exhausted). Callers branch on it exactly as they
	// branched on the old single resp.
	Resp LLMResponse
	// Content is the assistant content accumulated across every attempt
	// (continuation keeps partial output — design D2), so the terminal
	// finalContent is the seamless concatenation.
	Content string
	// Usage is the token usage summed across every attempt, including
	// length-cut ones (real spend — folded into total/by_agent). For the
	// end-of-round context observation (context_tokens) use Resp.Usage —
	// the final attempt's request is the authoritative next-request size
	// (design D8).
	Usage Usage
	// LengthHit reports that the final attempt still ended
	// finish_reason=length. It is a plain fact about the response: callers
	// gate the exhaustion path on their LengthContinueConfig.Enabled() so a
	// disabled deployment keeps the silent-terminal behavior.
	LengthHit bool
}

// runLLMRound runs one logical LLM round: a StreamChat call, plus — while the
// response ends finish_reason=length and the configured continuation budget
// allows — the continuation loop: scaffold the round (assistant partial +
// user instruction, or assistant-with-half-calls + per-call triage), expand
// req.MaxTokens by ExpandStep, rebuild req.Messages from the mutated round,
// and re-request. With the capability disabled (lc zero) it degenerates to a
// single StreamChat call — byte-for-byte prior loop behavior.
//
// req is the caller-built request for the round (Messages = rebuild(round) at
// entry); the helper owns only req.MaxTokens and req.Messages across
// attempts. round is the caller's conversation slice (pointer — scaffolding
// appends must persist for the rest of the turn); rebuild maps a round to the
// full messages array (the loops use withSystem; the wrap-up path appends its
// steering instruction on top). dispatch (optional) executes the parseable
// tool_calls of an intermediate length response. onToken/onReasoning are the
// caller's streaming closures, invoked for every attempt — the continuation
// stream is seamless by design (no event between attempts).
func runLLMRound(ctx context.Context, client LLMClient, hub stream.EventHub, agentName string,
	req LLMRequest, round *[]Message, lc config.LengthContinueConfig,
	rebuild func([]Message) []Message, dispatch lengthRoundDispatch,
	onToken, onReasoning func(string) error) (roundResult, error) {

	step, maxRetries := lc.Resolve() // (0, 0) when disabled
	baseTokens := req.MaxTokens
	var res roundResult
	for attempt := 0; ; attempt++ {
		resp, err := client.StreamChat(ctx, req, onToken, onReasoning)
		if err != nil {
			res.Resp = resp
			return res, err
		}
		res.Resp = resp
		res.Content += resp.Content
		res.Usage.Add(resp.Usage)
		if resp.FinishReason != "length" || attempt >= maxRetries {
			res.LengthHit = resp.FinishReason == "length"
			return res, nil
		}

		// finish_reason=length with continuation budget left: keep the
		// partial output and re-request with an expanded budget.
		logger.L().Warn("LLM output hit the length limit; continuing with expanded budget",
			zap.String("event", "llm_length_continuation"),
			zap.String("agent", agentName),
			zap.Int("continuation", attempt+1),
			zap.Int("max_continuations", maxRetries))
		scaffoldLengthRound(ctx, hub, agentName, round, resp, dispatch)
		req.MaxTokens = baseTokens + (attempt+1)*step
		req.Messages = rebuild(*round)
	}
}

// scaffoldLengthRound appends the continuation scaffolding for a length
// response to the round (B-call-1, design D4/D5):
//
//   - The assistant message is appended verbatim — partial content AND any
//     half-truncated tool_calls — so the model sees its own output and
//     continues from it.
//   - No tool_calls: a user-role continuation instruction follows.
//   - With tool_calls: per-call triage. Arguments that parse as JSON dispatch
//     through the callback (their real results are appended by the caller's
//     dispatch-and-record closure); truncated arguments (including an empty
//     string — every registry tool requires at least `path`, so an args-less
//     call could never dispatch anyway) get a synthetic not-executed tool
//     result plus the sanitized tool_call/tool_result SSE pair. Every
//     tool_call of the message is answered before the continuation request
//     is sent, keeping the wire protocol valid.
func scaffoldLengthRound(ctx context.Context, hub stream.EventHub, agentName string,
	round *[]Message, resp LLMResponse, dispatch lengthRoundDispatch) {
	*round = append(*round, Message{
		Role:            "assistant",
		Content:         resp.Content,
		ReasoningContent: resp.ReasoningContent,
		ToolCalls:       resp.ToolCalls,
	})
	if len(resp.ToolCalls) == 0 {
		*round = append(*round, Message{Role: "user", Content: lengthContinueInstruction})
		return
	}

	var dispatchable []ToolCall
	for _, tc := range resp.ToolCalls {
		if json.Valid([]byte(tc.Function.Arguments)) {
			dispatchable = append(dispatchable, tc)
			continue
		}
		msg := truncatedToolCallResult(tc.Function.Name)
		// Sanitized pair: the truncated arguments are invalid JSON, so the
		// tool_call event carries {} to keep the SSE payload parseable.
		hub.SendCtx(ctx, stream.ToolCallEvent(agentName, tc.ID, tc.Function.Name, json.RawMessage("{}")))
		hub.SendCtx(ctx, stream.ToolResultEvent(agentName, tc.ID, msg))
		*round = append(*round, Message{Role: "tool", Content: msg, ToolCallID: tc.ID, Name: tc.Function.Name})
	}
	if len(dispatchable) == 0 {
		return
	}
	if dispatch != nil {
		dispatch(ctx, dispatchable)
		return
	}
	// Wrap-up path: tools are stripped, so even complete calls cannot be
	// honored; answer them synthetically to keep the protocol valid.
	for _, tc := range dispatchable {
		hub.SendCtx(ctx, stream.ToolCallEvent(agentName, tc.ID, tc.Function.Name, json.RawMessage(tc.Function.Arguments)))
		hub.SendCtx(ctx, stream.ToolResultEvent(agentName, tc.ID, wrapUpToolCallNotExecutableResult))
		*round = append(*round, Message{Role: "tool", Content: wrapUpToolCallNotExecutableResult, ToolCallID: tc.ID, Name: tc.Function.Name})
	}
}

// lengthExhaustedMessage renders the agent_error text for the exhaustion
// path (design D7). Deliberately free of transient-error substrings
// ("timeout", "429", ...) so isTransientError never routes a length-exhausted
// sub-agent failure into the transient retry pipeline.
func lengthExhaustedMessage(agentName string, continuations, finalBudget int) string {
	return fmt.Sprintf("%s: output length limit reached after %d continuation attempt(s) (final budget %d tokens); the response is incomplete", agentName, continuations, finalBudget)
}
