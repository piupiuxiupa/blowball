# Design: add-llm-stream-idle-watchdog

## Context

`OpenAIClient.StreamChat` (`internal/agent/openai_client.go:177`) is the single
production implementation of `LLMClient`. Its read loop — `for stream.Next()` —
blocks on the SSE reader with no deadline; the only unblock conditions are a
frame arriving, the SDK surfacing an error, or the caller's context being
canceled. The 2026-08-18 incident showed the fourth state: the gateway stops
sending bytes while keeping the connection open. The turn hangs until the
browser disconnects, which is a user action, not a system property.

Every LLM call in the process flows through this one method: the three agent
tool-loops, round-cap wrap-up rounds, title generation (on a
`Background`-derived context — uncancellable today), and compaction summary
generation. Downstream, `internal/agent/retry.go` classifies transient errors
by substring (`"timeout"`, `"connection reset"`, …) and the sub-agent dispatch
retry path (policy / budget / idempotency tracker / backoff) already handles
transient failures — it just never sees this one, because silence never becomes
an error.

## Goals / Non-Goals

**Goals:**

- Bound every streaming LLM call by an inter-frame idle gap (including
  time-to-first-frame), configurable via `openai.stream_idle_timeout`.
- Convert a stall into a typed, transient-classified error so the existing
  retry machinery engages unchanged.
- Keep the abort observable: WARN log + `llm_raw_log` `kind=error` row with
  gap/frames diagnostics.
- Zero behavior change when the field is unset (opt-in, like `tools.timeouts`).

**Non-Goals:**

- No total-duration cap on a call (a legitimately long generation is fine; only
  silence is pathological).
- No changes to `retry.go`, the retry policy, budget, or SSE event schema.
- No request-level timeout for non-streaming endpoints (none exist).
- No automatic re-dispatch of side-effecting sub-agents that already executed
  tools — the idempotency tracker's existing verdict stands.

## Decisions

### D1: Watchdog lives inside `OpenAIClient.StreamChat`

One choke point covers all six call shapes (agents, wrap-up, title, compaction).
The stall is a transport-level property of the SSE read, and the agent-layer
retry classifier is deliberately SDK-agnostic (`retry.go` imports no openai-go
types), so detection belongs next to `stream.Next()`, classification stays
string-based.
*Alternative rejected:* a per-agent wrapper — would multiply configuration and
silently miss title/compaction calls, the two that hang *hardest* (title runs
on a context nothing else can cancel).

### D2: Derived cancel context + resettable timer goroutine

```go
callCtx, cancel := context.WithCancel(ctx)
defer cancel()
// NewStreaming(callCtx, ...) instead of (ctx, ...)

reset := make(chan struct{}, 1)     // watchdog kick channel
var fired atomic.Bool
go func() {
    timer := time.NewTimer(idle)
    defer timer.Stop()
    for {
        select {
        case <-timer.C:
            fired.Store(true)
            cancel()                  // unblocks stream.Next()
            return
        case <-reset:
            timer.Reset(idle)         // drained-and-reset pattern, single owner
        case <-callCtx.Done():
            return                    // normal end or parent cancel — always exits
        }
    }
}()
```

The read loop performs a non-blocking kick after every accepted frame
(`select { case reset <- struct{}{}: default: }`); the buffered-1 channel
coalesces bursts, so kicks never block the hot path and a kick racing the timer
fire resolves next loop iteration. The goroutine cannot leak: it selects on
`callCtx.Done()`, and `cancel` is deferred on the caller's path.
*Alternative rejected:* `http.Client.Timeout` (bounds the whole exchange — kills
legitimate long generations) or `net.Conn.SetReadDeadline` (the conn is buried
under the SDK's decompress/SSE readers; a custom transport is invasive).
*Acceptable equivalent:* a ticker goroutine polling an atomic `lastFrame`
timestamp — same observable behavior; implementer's choice, tests must not
distinguish the two.

### D3: Parent-cancellation precedence and the typed error

After the read loop exits, in order:

1. `ctx.Err() != nil` (parent canceled: client disconnect, shutdown) → existing
   behavior untouched: `capturePartial()` + return `ctx.Err()` (raw-capture
   design D6). A canceled request cannot use a retry anyway, so parent wins any
   race with the timer.
2. else `fired` is set → `captureError(err)` + return the typed error.
3. else → existing stream-error / success paths.

Typed error:

```go
var ErrStreamIdleTimeout = errors.New("openai client: stream idle timeout")
return fmt.Errorf("%w: no frame for %s (frames=%d, model=%s)",
    ErrStreamIdleTimeout, idle, frameIdx, req.Model)
```

The message contains "timeout" → `isTransientError` matches → `shouldRetry`
decides per existing policy. Errors-wrapping keeps `errors.Is` available for
tests and future callers.

### D4: Timer reset on any accepted frame

Any frame — empty delta, usage-only frame, reasoning delta — proves the stream
is alive; resetting on all of them is cheap and conservative. The timer starts
*before* `NewStreaming`, so a gateway that completes the HTTP handshake but
never sends a first frame is covered (time-to-first-frame is just the first
inter-frame gap).

### D5: Config — `openai.stream_idle_timeout`, zero = disabled

- `OpenAIConfig.StreamIdleTimeout time.Duration` with `yaml:"stream_idle_timeout"`;
  duration suffixes and `${VAR}` expansion apply as everywhere else.
- `0`/absent → watchdog disabled, byte-for-byte prior behavior (the
  `tools.timeouts` opt-in convention). Negative → config load fails fast.
- `config.example.yaml` documents `2m` as the recommended value with rationale
  (30ms-cadence streams go silent on stall; thinking-model streams still emit
  reasoning deltas continuously, so multi-minute silence is abnormal on every
  gateway blowball targets).
- Rationale for opt-in rather than default-on: thinking models behind
  non-reasoning-streaming gateways can legitimately be silent for minutes; a
  wrong default would abort healthy turns. Operators who know their gateway
  enable it. Revisit with real-world data (Open Question).

### D6: Raw capture — watchdog abort is a `kind=error` row

`captureError` path, so: `kind=error`, `http_status=0`, `finish_reason` = raw
finish so far (usually empty), `raw` = the typed error message (gap, frames,
model — the post-mortem numbers). The last `kind=chunk` row's `msg_time` vs the
error row's shows the measured gap from the wire. Distinct from local context
cancellation, which stays a partial `kind=response` row — cancellation is "the
caller walked away" (no fault), idle timeout is "the stream broke" (fault).
*Alternative rejected:* a new `kind=idle_timeout` value — fragments the
four-value `kind` domain shared by queries for no filter win (`kind='error' AND
http_status=0` already isolates it).

### D7: Retry integration — deliberately zero changes

`shouldRetry` + `retryBudget` + `ToolCallTracker` + `computeBackoff` operate on
the returned error as-is. Consequences worth stating:

- Liang (retry default-enabled): an idle-stalled Liang call retries under the
  existing budget.
- Chongzhi (retry default-disabled; even enabled, only pre-tool-call): a
  mid-generation stall surfaces to Confucius as the tool-failure envelope
  (`status:1`), and Confucius can re-dispatch as a *new* tool call — model-level
  recovery instead of mechanical retry. Correct for a side-effecting agent.
- Confucius's own loop and title: no mechanical retry today; they fail fast with
  a clear error. Turn failure with reason ≫ silent 8-minute hang.

### D8: Observability on fire

One WARN per watchdog fire: agent name (from ctx), model, frames received,
configured idle, elapsed. No per-reset logging (hot path). No new SSE event:
the failure travels the existing `agent_error` / tool-envelope routes.

## Risks / Trade-offs

- [False positive on a legitimately-silent gateway] → default off; per-deploy
  tuning; the error message carries frames+gap so misfires are diagnosable from
  `llm_raw_log`.
- [Watchdog goroutine leak] → it selects on `callCtx.Done()` and `cancel` is
  deferred; leak is structurally impossible. Covered by a leak-test.
- [Cancel-source race (user disconnects as the timer fires)] → D3's
  parent-first precedence; cancellation semantics preserved.
- [`timer.Reset` misuse pitfall] → single-owner drain-and-reset in the watchdog
  goroutine only; never Reset from the read loop.
- [Retry amplification during a hard gateway outage] → unchanged exposure:
  bounded by MaxAttempts, backoff cap, and the per-turn retry token budget that
  already exist.

## Migration Plan

Config-only rollout: set `openai.stream_idle_timeout: 2m` and restart. Rollback:
unset the field and restart — no schema, queue, or wire-format coupling. Safe
to deploy with mixed versions in a multi-process role split (each process reads
its own config; the watchdog is per-call and needs no coordination).

## Open Questions

- Default value: keep opt-in (`0`) vs ship `2m` enabled by default once we have
  gap telemetry from `llm_raw_log` on real deployments. Decide after ~a week of
  production data with the field enabled.
- Should the title-generation context also get a total-duration bound (it runs
  on `Background`)? The idle watchdog bounds the common case; a total bound is
  a separate, tiny follow-up if titles ever hang on an endless-but-alive
  stream.
