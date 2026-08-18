package agent

import "context"

// RoundHook is the between-rounds seam the orchestration wiring injects into
// the Confucius tool-calling loop (context-compaction capability, mid-turn
// trigger). It is invoked after a round's tool results have been appended to
// the in-memory conversation and before the next round's LLM request, and
// carries the just-finished round's authoritative usage — whose
// PromptTokens+CompletionTokens is the true size the NEXT request's context
// will reach (system prompt + tools + full history, measured by the provider,
// zero estimation error).
//
// The hook implementation owns the turn's collected event stream (the wiring
// binds it to the orchestrator adapter's synchronized event tap) plus the
// flush-first persistence and the CompactionService call, so the agent layer
// itself never depends on the service layer. Its contract:
//
//   - It must not emit SSE events — compaction is silent by spec (the SSE
//     stream observes it only as a pause while the summary call runs).
//   - It returns a replacement conversation when a mid-turn compaction
//     rewrote the context (first message + framed summary + retained tail);
//     a nil return keeps the loop's conversation unchanged (threshold not
//     reached, empty middle, or any best-effort failure).
//   - It must be quick to return nil when the threshold is not reached; the
//     loop calls it after EVERY dispatched tool round.
type RoundHook func(ctx context.Context, lastUsage Usage) []Message

// RoundHookSetter is the optional capability an agent exposes when its Run
// supports a between-rounds hook. Only Confucius implements it (sub-agents
// never see the parent conversation, so mid-turn pressure lives in the
// dispatcher's loop alone). The orchestrator installs the per-request hook on
// the freshly-built agent right after the factory constructs it, so no state
// leaks across turns.
type RoundHookSetter interface {
	SetRoundHook(hook RoundHook)
}
