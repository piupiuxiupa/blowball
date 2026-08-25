package agent

import (
	"context"
	"fmt"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/stream"
	"go.uber.org/zap"
)

// wrapUpInstruction is appended to the wrap-up round's messages to steer the
// model away from further tool use (the round carries no tools anyway) and
// toward synthesizing a final answer from what the conversation has already
// gathered. Without it the model keeps following its prior tool-use flow and
// — on gateways that echo learned tool-use even with an empty tools[] — emits
// tool_calls we cannot honor, whose accompanying prose is just a "I'll call X
// next" preamble rather than a real answer.
const wrapUpInstruction = "The tool-call round limit for this task has been reached. No further tool calls are possible in this round. Using only the information already gathered above, write your final response to the user now. If the task cannot be completed with what is available, say so briefly."

// runWrapUpRound runs a single tool-disabled LLM round that lets the model
// synthesize a final answer after its tool-calling loop hit the max_rounds
// cap. req MUST already have Tools cleared (and, for a structured-output agent,
// ResponseFormat set). rebuild maps the caller's round to the wrap attempt's
// full messages array — the caller appends wrapUpInstruction on top of its
// system prompt, and runWrapUpRound applies rebuild at entry and again after
// every length continuation (llm-length-continuation covers the wrap-up round;
// continuation attempts keep the steering instruction as the trailing
// message). retry is the OWNING agent's retry policy — the wrap-up round's
// StreamChat calls get the same round-level transient retry as the main loop
// (llm-round-retry). Content and reasoning chunks are streamed to hub exactly
// like a normal round. It performs no tool dispatch and runs exactly one
// round.
//
// Returns the synthesized content, the round's usage, and any error. If the
// FINAL attempt emits tool_calls (some OpenAI-compatible gateways do even
// with tools stripped), they cannot be honored and the prose is a preamble,
// not an answer — so the empty string is returned to route the caller onto
// its empty-end (round_cap_exhausted) path instead of silently returning a
// useless non-answer. Length exhaustion likewise surfaces as an error so the
// caller's round_cap_exhausted path applies (the length_exhausted code belongs
// to the main loops, which own their agent_error emission).
func runWrapUpRound(ctx context.Context, client LLMClient, agentName string, hub stream.EventHub,
	req LLMRequest, round *[]Message, lc config.LengthContinueConfig, retry config.AgentRetryConfig,
	rebuild func([]Message) []Message) (string, Usage, error) {
	req.Messages = rebuild(*round)

	var streamed string
	result, err := runLLMRound(ctx, client, hub, agentName, req, round, lc, retry, rebuild, nil,
		func(delta string) error {
			streamed += delta
			if !hub.SendCtx(ctx, stream.TokenEvent(agentName, delta)) {
				return ctx.Err()
			}
			return nil
		}, func(delta string) error {
			if !hub.SendCtx(ctx, stream.ReasoningEvent(agentName, delta)) {
				return ctx.Err()
			}
			return nil
		})
	if err != nil {
		return streamed, result.Usage, err
	}
	resp := result.Resp
	// tool_calls here cannot be honored (tools were stripped); treat as no
	// usable synthesized content so the caller fails loudly rather than
	// returning the preamble as the final answer.
	if len(resp.ToolCalls) > 0 {
		return "", result.Usage, nil
	}
	if lc.Enabled() && result.LengthHit {
		return "", result.Usage, fmt.Errorf("length exhausted after continuation (final budget %d tokens)", req.MaxCompletionTokens)
	}
	// Match the main loop's precedence: prefer the content accumulated across
	// the round's attempts (continuation keeps partial output — the final
	// attempt's Content is only its suffix), falling back to the
	// streamed-token accumulation only when the client did not populate
	// Content on any attempt (some OpenAI-compatible endpoints leave it
	// empty).
	content := result.Content
	if content == "" {
		content = streamed
	}
	return content, result.Usage, nil
}

// emitCapHitWarn logs (operator-side) that an agent hit its round cap and is
// running a wrap-up round. Emitted on every cap-hit across all three agents so
// cap frequency is observable even when the wrap-up round recovers a usable
// answer (the success path emits no agent_error). This is the always-on cap
// signal; the user-facing agent_error fires only on the genuine empty-end path
// (see emitCapExhaustedError).
func emitCapHitWarn(agentName string, cap, executed int) {
	logger.L().Warn("agent round cap reached; running wrap-up round",
		zap.String("agent", agentName),
		zap.Int("max_rounds", cap),
		zap.Int("rounds_executed", executed),
	)
}

// emitCapExhaustedError emits the user-facing cap-exhausted signal: an
// agent_error (code round_cap_exhausted) followed by agent_end. Called only on
// the genuine empty-end path — when the wrap-up round failed to recover any
// content (LLM error or empty response), the case that previously ended the
// turn silently with an empty answer. The caller returns a non-nil error so
// the orchestrator's done event additionally carries an `error` field.
func emitCapExhaustedError(ctx context.Context, hub stream.EventHub, agentName string) {
	hub.SendCtx(ctx, stream.AgentErrorEvent(agentName, "round cap exhausted: wrap-up round produced no content", "round_cap_exhausted"))
	hub.SendCtx(ctx, stream.AgentEndEvent(agentName))
}
