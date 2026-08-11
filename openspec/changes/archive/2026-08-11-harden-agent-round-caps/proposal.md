## Why

Each agent's tool-calling loop is bounded by a hardcoded round cap (`maxConfuciusRounds` / `maxChongzhiRounds` / `maxLiangRounds`). When the cap is reached the loop simply falls through: it returns whatever `finalContent` it has — which is almost always the empty string, because the last round had just dispatched `tool_calls` and produced no prose — with `err == nil`, no `agent_error` event, no log line, and no chance for the model to summarize. A capped turn therefore looks identical to a healthy one on the wire except the final answer is empty: an "agent went silent" mystery that is invisible to operators and untunable. The cap value itself is a compile-time constant, so operators cannot tighten or loosen it per agent without a rebuild.

## What Changes

- **Configurable per-agent round cap.** A new `agents.<name>.max_rounds` field (int) replaces the three hardcoded constants. Unset / `<= 0` falls back to a shared default of `100`, reproducing the current working-tree behavior exactly (zero behavior change for operators who omit the field). The three package constants are removed.
- **Cap-hit signal (observability).** Every cap-hit exit emits a structured `WARN` log (agent, cap, rounds executed). Cap-hit turns additionally set an additive `usage.meta.round_capped = true` on the terminal `done` event so cap frequency is queryable in `turn_usage` without mislabeling the turn as failed.
- **Cap-hit wrap-up round.** On cap exhaustion the loop no longer terminates empty. It runs one additional LLM round with **tools disabled** (and, for Confucius, no further sub-agent dispatch) so the model is forced to emit a final prose answer synthesizing the conversation so far; that answer becomes the turn's `finalContent`. Tokens from the wrap-up round are counted normally.
- **Loud signal only when the cap actually costs the turn.** If the wrap-up round yields usable content, the turn ends as a normal success (the `WARN` log + `meta.round_capped` are the only cap signals — no error UI). If the wrap-up round yields no content or errors, the loop emits `agent_error` (code `round_cap_exhausted`) + `agent_end` and the `done` event carries `error: "..."` — the previously-silent empty-end case is now diagnosable.

This applies uniformly to all three loops (Confucius, Chongzhi, Liang); shared wrap-up/signal logic is factored into a helper to avoid triplication.

## Capabilities

### New Capabilities
<!-- None — all changes live under the existing agent-orchestration capability. -->

### Modified Capabilities
- `agent-orchestration`: the per-agent tool-calling loop gains a configurable round cap (`max_rounds`), observable cap-hit signaling (log + `usage.meta.round_capped`, plus `agent_error`/`done.error` on genuine empty-end), and a single tool-disabled wrap-up round before termination. `Agent configuration from file` is extended to load `max_rounds`; `Confucius agent loop` is extended to acknowledge cap-bounded termination (the sub-agent loops gain the same semantics).

## Impact

- **Code**: `internal/agent/confucius.go`, `internal/agent/chongzhi.go`, `internal/agent/liang.go` (replace the three `max*Rounds` constants with the config field; add cap-detection + wrap-up round + signal emission; factor a shared helper); `internal/agent/orchestrator.go` / `buildConfucius` / `buildChongzhi` / `buildLiang` (thread `cfg.MaxRounds` into the agents, applying the default when unset); `internal/agent/agent.go` (`turnMeta` gains a `roundCapped` flag folded into the done `meta` object); `internal/config/config.go` (`AgentConfig.MaxRounds` field + validation: reject `< 0`, treat `0`/unset as default).
- **Config**: `agents.<name>.max_rounds` (optional int, default `100`); `config.example.yaml` gains an example entry for at least one agent.
- **SSE / done contract**: additive only — a new optional `usage.meta.round_capped` boolean, and `agent_error` (code `round_cap_exhausted`) + `done.error` on the genuine empty-end path. No existing event type, route, or the `{total, by_agent, meta}` shape is removed or renamed; frontend consumers ignore unknown `meta` keys and treat `round_cap_exhausted` like any other `agent_error`.
- **Persistence**: unchanged — the wrap-up round's tokens fold into the existing per-agent usage attribution; `turn_usage.usage_json` carries the new `meta.round_capped` key transparently (it stores the usage object verbatim).
- **No model-facing tool contract change**: tool schemas and the tool-result envelope are untouched; the wrap-up round only omits `tools` from the request.
