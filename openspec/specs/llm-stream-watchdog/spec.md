# llm-stream-watchdog Specification

## Purpose

Streaming LLM calls through the shared LLM client are bounded by a configurable inter-frame idle timeout (`openai.stream_idle_timeout`, opt-in like `tools.timeouts`): the gap timer starts when a call is issued (covering time-to-first-frame) and resets on every accepted SSE frame, so a stalled stream aborts with a typed error (`ErrStreamIdleTimeout`) instead of hanging a turn forever. The error is transient-classified by the existing sub-agent retry path with no retry-logic changes, caller cancellation always wins over the watchdog, coverage spans every streaming call (agent tool-loops, round-cap wrap-up rounds, title generation, compaction summaries) with exactly one WARN log per fire, and an unset/zero config value keeps byte-for-byte prior behavior.

## Requirements

### Requirement: Inter-frame idle timeout bounds streaming LLM calls
When `openai.stream_idle_timeout` is configured, the LLM client SHALL bound every streaming chat call by the maximum gap between consecutive SSE frames, where the first gap starts when the call is issued (covering time-to-first-frame). Any accepted frame — including empty deltas, usage-only frames, and reasoning deltas — SHALL reset the gap timer, and the call's total duration SHALL NOT be bounded. When the gap is exceeded, the client MUST abort the stream and return a typed error (`ErrStreamIdleTimeout`) whose message includes the word "timeout" plus the configured idle, frames received, and model name.

#### Scenario: Stream stalls mid-generation
- **WHEN** a streaming call has been delivering frames and then produces no frame for longer than `openai.stream_idle_timeout`
- **THEN** the call returns the typed idle-timeout error (not a hang, not a partial success) carrying frames received, the configured idle, and the model

#### Scenario: Gateway never sends the first frame
- **WHEN** the HTTP handshake succeeds but zero frames arrive within `openai.stream_idle_timeout` of the call being issued
- **THEN** the call returns the typed idle-timeout error

#### Scenario: Long but alive generation is never aborted
- **WHEN** a streaming call delivers frames continuously for a total duration far exceeding `openai.stream_idle_timeout`
- **THEN** the call proceeds normally; only inter-frame silence, never total duration, triggers the watchdog

### Requirement: Watchdog is disabled without configuration
The watchdog SHALL be disabled when `openai.stream_idle_timeout` is unset or zero, producing byte-for-byte prior streaming behavior. A negative value MUST be rejected at config load. The field SHALL support duration suffixes and `${VAR}` expansion like every other config string.

#### Scenario: Unset config keeps prior behavior
- **WHEN** `openai.stream_idle_timeout` is unset or `0`
- **THEN** a stalled stream blocks the read loop indefinitely exactly as before the capability, and no watchdog goroutine influences call outcomes

#### Scenario: Negative value fails config load
- **WHEN** the config sets `openai.stream_idle_timeout: -1s`
- **THEN** config loading fails with an explicit error

### Requirement: Idle-timeout errors are transient-classified for sub-agent retry
The idle-timeout error SHALL match the existing transient-error classifier (`isTransientError`) via its "timeout" message so that the existing sub-agent retry path — policy enablement, per-turn token budget, side-effect idempotency tracker, exponential backoff — decides retryability with no retry-logic changes.

#### Scenario: Retryable agent stalls and retries through the existing path
- **WHEN** a sub-agent with retry policy enabled (and no executed tool call yet) fails with the idle-timeout error
- **THEN** the dispatcher retries it under the existing budget/backoff and emits the existing retry-signaling `agent_error` event (`Meta.retry=true`)

#### Scenario: Side-effecting agent is not mechanically retried
- **WHEN** a sub-agent that has already executed a tool call fails with the idle-timeout error
- **THEN** no mechanical retry occurs; the failure surfaces to the orchestrator through the tool-result status envelope so the model may re-dispatch as a new call

### Requirement: Caller cancellation takes precedence over the watchdog
The call SHALL report the caller's context cancellation — client disconnect or shutdown, with the existing partial-response capture behavior — even when the idle timer fires concurrently. The watchdog SHALL NOT alter any existing cancellation semantics.

#### Scenario: Client disconnects while the timer fires
- **WHEN** the SSE client disconnects at the same moment the idle gap is exceeded
- **THEN** the call returns `context.Canceled` with the partial-response capture row, identical to pre-capability cancellation behavior

### Requirement: Watchdog coverage spans every streaming call
The watchdog SHALL apply to every call made through the shared LLM client: the three agent tool-loops, round-cap wrap-up rounds, title generation, and compaction summary generation. Each watchdog fire MUST emit one WARN log carrying the agent name, model, frames received, and configured idle. The watchdog goroutine MUST NOT outlive its call.

#### Scenario: Title-generation call is bounded
- **WHEN** a title-generation stream (running on a background-derived context that nothing else can cancel) stalls past the configured idle
- **THEN** the call aborts with the typed error and the goroutine exits

#### Scenario: Watchdog fire is logged
- **WHEN** the watchdog aborts a call
- **THEN** exactly one WARN entry is logged with agent, model, frames received, and the configured idle value
