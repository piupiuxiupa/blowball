## Context

Each agent runs a tool-calling loop of the shape `for i := 0; i < maxRounds; i++ { StreamChat → dispatch tool_calls }` (`internal/agent/confucius.go`, `chongzhi.go`, `liang.go`). `finalContent` is assigned only on the two natural-exit `break` branches (empty assistant turn, or `!shouldDispatchToolCalls`). Consequently, when the loop exhausts `maxRounds` while the model keeps emitting `tool_calls`, it falls through with `finalContent == ""` and `err == nil`, then emits a plain `agent_end` + `done`. The cap is invisible: no log, no `agent_error`, no `done.error`, no chance to summarize — an "agent went silent" state that is undiagnosable and untunable.

The three caps are compile-time constants. The working tree currently sets all three to `100` (operator-edited from the historical `16`/`32`/`16`). This change makes them configurable, makes cap-hits observable, and gives the model one tool-disabled round to recover a usable answer before the turn ends.

The loop is mirrored across the three agents rather than shared, and the turn already carries a `turnMeta` struct (`parallel`, `sub_agent_invocations`) rendered into `done`'s `usage.meta` via `buildMetaObject`. Per-agent usage attribution and the `done` `{total, by_agent, meta}` shape are untouched except for one additive `meta` key.

## Goals / Non-Goals

**Goals:**
- Make the per-agent round cap operator-configurable (`agents.<name>.max_rounds`), defaulting to the current working-tree value so omission is zero behavior change.
- Make every cap-hit observable to operators (structured `WARN` log) and to analytics (`usage.meta.round_capped` on `done`, persisted into `turn_usage`).
- Give the model one bounded, tool-disabled "wrap-up" round to synthesize a final answer on cap exhaustion, so a capped turn usually still returns content instead of empty.
- Preserve the existing `agent_error`/`done.error` failure semantics: the loud signal fires only when the cap actually costs the turn (wrap-up yields nothing).
- Apply uniformly to all three loops via a shared helper (no triplicated logic).

**Non-Goals:**
- No per-tool-call counting cap. The bound is on LLM rounds, not tool invocations (a single round may fan out many parallel `tool_calls`). Out of scope.
- No per-turn wall-clock timeout (ctx-cancellation behavior is unchanged).
- No change to sub-agent retry policy, usage attribution, or the tool-result envelope.
- No dynamic/elastic cap (cap does not auto-grow mid-turn).
- The wrap-up round is always on for capped loops; an opt-out is out of scope.

## Decisions

### Decision 1 — Default `max_rounds = 100`, applied at agent construction
Unset / `<= 0` resolves to `100`, reproducing today's working tree exactly. Applied in the agent builders (`buildConfucius` / `buildChongzhi` / `buildLiang` in `orchestrator.go`), mirroring how `applyRetryDefaults` seeds per-agent retry defaults — the config layer stays agnostic to agent semantics and the agent self-defaults. The three package constants (`maxConfuciusRounds` etc.) are removed.
- *Alternative considered:* restore the historical differentiated defaults (`16`/`32`/`16`). Rejected — it would change behavior for operators who omit the field, violating zero-behavior-change and contradicting the operator's manual `100` edit. Those values remain documented here as the pre-edit baseline; operators who want them set `max_rounds` explicitly.

### Decision 2 — Cap-detection distinguishes natural exit from cap exhaustion
The wrap-up/signal path triggers **only** when the loop exits because `i == maxRounds` while the last response still wanted tools (i.e. not via a `break`). Tracked with a `capped bool` set when the loop body completes a dispatch round without `break` and `i+1 == maxRounds`. Natural exits (`finish_reason=stop`, empty assistant turn) bypass wrap-up entirely and emit no cap signal.
- *Alternative:* trigger on any loop exit. Rejected — would fire on healthy turns.

### Decision 3 — Wrap-up round disables tools; treated as a terminal round
On cap exhaustion, run exactly one more `StreamChat` with **no `tools`** in the request (and, for Confucius, no `dispatchToolCalls`/sub-agent path — the dispatch switch is skipped because there are no `tool_calls` to dispatch). The model can only return prose (+ reasoning if `thinking`), which becomes `finalContent`. Its tokens fold into the agent's usage normally. The round is strictly single-shot: whatever it returns terminates the loop.
- For **Liang** with `output_schema`, the wrap-up round IS the terminal round, so `terminalResponseFormat` still applies (structured output on the synthesis round). This falls out for free because the wrap-up is constructed the same way as a normal terminal round, minus tools.
- *Alternative considered:* allow the wrap-up round to keep tools so the model could "finish one more step". Rejected — it would re-enter the tool loop and defeat the cap.

### Decision 4 — Layered signaling (observability vs. failure)
- **Every cap-hit:** structured `WARN` log (`agent`, `max_rounds`, `rounds_executed`) + `usage.meta.round_capped = true` (additive key on `done`, stored verbatim in `turn_usage.usage_json`). No UX impact.
- **Genuine empty-end only** (wrap-up produced no usable content or errored): emit `agent_error` (code `round_cap_exhausted`) + `agent_end`, and set `done.error`. This is the previously-silent failure made loud.
- *Alternative considered:* fire `agent_error` + `done.error` on every cap-hit unconditionally. Rejected — it would mislabel salvaged (successful) turns as failures and trigger error UI for turns that actually returned a good answer. The `meta.round_capped` flag + log give always-on observability without that downside, satisfying "signal on every cap-hit" without the UX cost.

### Decision 5 — Shared helper for wrap-up + signal
Extract one helper (e.g. `runWrapUpRound` + a `capHitSignal` emitter) used by all three loops, rather than copy-pasting the mechanic three times. The loops themselves stay structurally as-is (each owns its `for` + dispatch), matching the existing mirroring style; only the cap-exit tail is shared.

### Decision 6 — `meta.round_capped` is additive and optional on the wire
`turnMeta` gains a `roundCapped bool`; `buildMetaObject` includes `round_capped` only when `true` (omitted otherwise, so non-capped turns serialize identically to today). Frontend and `turn_usage` readers already tolerate arbitrary `meta` keys; no consumer breaks.

### Decision 7 — Wrap-up round steers the model and rejects tool_calls (production fix)
Two defects surfaced in production (glm-5.2 at `max_rounds=2`): (1) stripping `tools` from the wrap-up request did not stop the model from emitting `tool_calls` — some OpenAI-compatible gateways echo learned tool-use even with an empty `tools[]`; (2) the wrap-up blindly accepted the accompanying prose ("I'll call X next") as the final answer, so a capped turn ended silently with a useless non-answer and no `agent_error`. Fix: `runWrapUpRound` now appends an explicit steering message ("round limit reached, no more tools, answer now") so the model synthesizes instead of continuing its tool-use flow, AND treats any `tool_calls` in the wrap-up response as no usable content (routing to the loud `round_cap_exhausted` path) since those calls cannot be honored and their prose is only a preamble. Covered by `TestChongzhi_RoundCap_WrapUpReturnsToolCalls_SurfacesError`.
- *Alternative considered:* feed the model additional rounds until it stops emitting tool_calls. Rejected — unbounded, defeats the cap.

## Risks / Trade-offs

- **[Wrap-up round adds one LLM call's latency + cost on every cap-hit]** → Bounded to exactly one round, and cap-hits are already a degenerate (runaway) case the operator wants to recover from; the recovered content is usually worth far more than one extra round. The `WARN` log makes the cost observable.
- **[Default `100` may be too high (cost) or too low (false caps) for some workloads]** → Now operator-tunable per agent; that is the point of the change. `WARN` log + `meta.round_capped` let operators right-size it from real data.
- **[`meta.round_capped` is a new done-event key]** → Additive, omitted on non-capped turns, ignored by tolerant consumers; documented in the SSE section of CLAUDE.md.
- **[Wrap-up round with `thinking`/reasoning models]** → No conflict (no `response_format` unless `output_schema` forces it on the terminal round, which is desired for Liang). Covered by reusing the normal terminal-round construction.
- **[Confucius cap vs. sub-agent caps are independent]** → A sub-agent hitting its own cap runs its own wrap-up and returns its synthesized text as the `tool_result` to Confucius; Confucius's own cap is separate. No new concurrency/race surface — each `Run` is independent.
- **[Risk of MODIFIED-spec partial copy]** → Mitigated by copying the full existing requirement blocks for `Agent configuration from file` and `Confucius agent loop` and editing in place.

## Migration Plan

- **Deploy:** ship the code + `config.example.yaml` example. Operators who omit `max_rounds` see no behavior change (default `100`). Operators who want the historical caps set them explicitly.
- **Data:** no migration. `turn_usage.usage_json` stores the usage object verbatim; the new `meta.round_capped` key is harmless to existing readers. Old persisted turns simply lack the key.
- **Rollback:** revert the code change. Configs with `max_rounds` become unknown-key YAML (Go decoding ignores unknown fields) — harmless. No data to unwind.

## Open Questions

- None blocking. The signal-layering (Decision 4) is the one judgment call; the chosen design delivers always-on observability (`log` + `meta.round_capped`) plus the loud failure signal only on genuine empty-end. If the operator later wants the loud signal on *every* cap-hit regardless of salvage, that is a one-line change to widen the `agent_error`/`done.error` condition.
