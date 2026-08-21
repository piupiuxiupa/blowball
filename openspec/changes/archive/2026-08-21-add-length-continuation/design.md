# Design: add-length-continuation

## Context

`OpenAIClient.StreamChat` already aggregates `finish_reason` into
`resp.FinishReason` (defaulting empty to `"stop"`,
`internal/agent/openai_client.go:494`) — but nothing downstream reads it. All
three agent loops (Confucius `internal/agent/confucius.go:155`,
Chongzhi/Liang in their `Run`s) and the round-cap wrap-up round
(`internal/agent/roundcap.go:32`) branch only on "has tool_calls" and
"has content". Three silent termination paths result today:

| `length` shape | Today's behavior |
|---|---|
| content only | truncated text delivered as the final answer, no signal |
| with `tool_calls` | `shouldDispatchToolCalls` → false (`agent.go:281`), turn ends on the preamble, truncated calls vanish |
| all empty (thinking ate the budget) | empty `finalContent`, silent empty answer |

Tokens stream live through `onToken` → hub → SSE, so by the time the final
chunk reveals `finish_reason=length`, the truncated output is already on the
user's screen. `MergeEvents` (`internal/handler/event_mapper.go:36`) merges
consecutive token events per `(type, agent, run_id)` — consecutive runs with
no intervening event fold into one persisted row.

Background incident (2026-08-20): glm-5.2 truncated long `tool_call`
arguments at `max_tokens: 8192` and reported `finish_reason=tool_calls`
(not `length`). This design deliberately keys recovery on honest `length`
only and addresses the lying-gateway case through prevention (write-budget
guidance + `append` mode), not runtime detection.

## Goals / Non-Goals

**Goals:**

- A `length` round never silently terminates a turn when the feature is on:
  the agent keeps the partial output and continues with an expanded budget.
- The SSE stream, persistence pipeline, and frontend need **zero** schema
  changes: continuation is invisible plumbing behind a seamless token stream.
- Prevention: steer writes below the truncation point, and make "write in
  chunks" literally executable (`append` mode).
- Opt-in, zero-behavior-change default (`openai.length_continue` unset).

**Non-Goals:**

- Detecting truncation behind a lying `finish_reason` (invalid-args triage
  exists *only inside* an already-`length` response — it is not a trigger).
- Covering title generation / compaction summaries (invisible truncation,
  benign; they bypass the hub).
- Regeneration/discard semantics, checkpoint-and-resume across turns, new SSE
  event types, frontend changes, migrations.

## Decisions

### D1 — Trigger: `resp.FinishReason == "length"`, nothing else

The client's aggregated `FinishReason` is the sole trigger, compared as the
raw gateway string. No error classification, no args validation as a trigger.
Rationale: the honest signal is unambiguous and free; the lying-gateway case
is mitigated upstream by prevention (D10/D11) rather than heuristics that
would false-positive on genuinely-malformed small args.

### D2 — Continue from the truncation point, not regenerate

On `length`: keep the partial output, append scaffolding to `round`
(D4/D5), bump `MaxTokens`, re-request; accumulate content across attempts.

Alternative (rejected): discard-and-regenerate with a marker event. It
re-pays the prompt on every attempt, forces a new "discard what you saw"
SSE marker + frontend handling, and splits one logical answer into multiple
persisted rows. Continuation streams seamlessly — the frontend just sees the
answer keep growing; `MergeEvents` folds attempts into one row because no
event intervenes between attempt N's last token and attempt N+1's first.

### D3 — The continuation loop lives in a shared agent-layer helper

New file `internal/agent/lengthcontinue.go`. One helper wraps the
`StreamChat` call site and owns: the attempt loop, `round` mutation
(scaffolding appends), `MaxTokens` expansion, content accumulation, usage
folding, and exhaustion signaling. Adopted at exactly four sites: the three
agent main loops and `runWrapUpRound`. Sketch:

```go
type LengthContinueConfig struct{ ExpandStep, MaxRetries int } // zero => disabled

type roundResult struct {
    Resp      LLMResponse // final attempt's response (post-continuation)
    Content   string      // content accumulated across ALL attempts
    Usage     Usage       // usage summed across ALL attempts
    LengthHit bool        // final attempt STILL ended length (=> exhausted path)
}

// runLLMRound runs one logical round: StreamChat; while finish_reason=length
// and continuations remain, append scaffolding to *round, expand MaxTokens,
// re-request. dispatch is invoked for parseable tool_calls of a length
// response (D5); nil for tool-less rounds (wrap-up).
func runLLMRound(ctx context.Context, client LLMClient, hub stream.EventHub,
    agentName string, req LLMRequest, round *[]Message, lc LengthContinueConfig,
    dispatch func(ctx context.Context, calls []ToolCall) map[string]toolResult,
    onToken, onReasoning func(string) error) (roundResult, error)
```

Rejected alternative: implementing the retry inside `OpenAIClient.StreamChat`.
The client cannot emit hub events (no marker, no `tool_call`/`tool_result`
pair), cannot dispatch complete calls, and folding usage inside the client
would corrupt the caller-side `observeRoundContext` semantics (D8). The
loops' per-round `finalContent = resp.Content` becomes
`finalContent = result.Content`; everything else in the loops is unchanged.

### D4 — Prose truncation scaffolding

```
round += assistant{Content: resp.Content, ReasoningContent: resp.ReasoningContent}
round += user{Content: continuationInstruction}
```

The instruction (English, matching existing steering text like
`wrapUpInstruction`): output was cut by the length limit; resume exactly
where it stopped, do not repeat already-written content; for large file
writes split into multiple smaller writes. It stays in `round` for the rest
of the turn (subsequent rounds see it — a useful reminder). The all-empty
`length` (thinking consumed the budget) is the same shape: an empty
assistant message is wire-legal, and the expanded budget gives thinking
room (glm requires `max_completion_tokens > thinking budget`).

### D5 — Truncated `tool_calls`: keep the half-calls, triage per call (B-call-1)

```
round += assistant{Content, ReasoningContent, ToolCalls: resp.ToolCalls}   // half-calls included
for each tool_call:
    if json.Valid(args):  dispatch normally (existing dispatch callback path,
                          real result + existing ToolCallEvent/ToolResultEvent)
    else:                 synthetic tool result:
                          round += tool{id, "not executed: arguments were truncated
                          by the output length limit; re-issue the call, splitting
                          large content into multiple smaller writes"}
                          SSE: ToolCallEvent (args sanitized to `{}`) + ToolResultEvent
```

- **Why dispatch inside the helper**: the wire protocol requires every
  assistant `tool_call` to be answered by a `tool` message before the next
  request — deferring dispatch of the parseable calls is not an option, and
  synthesizing "deferred" results for valid calls would waste them.
- **Why keep the half-calls in context** (B-call-1 over dropping them):
  the model sees its own partial arguments and continues from them; every
  call has an answer, so the wire stays valid. Risk: some gateways may
  reject invalid-JSON `arguments` strings on the way back — see Risks.
- **Triage test**: `json.Valid([]byte(tc.Function.Arguments))`. An empty
  args string is invalid and routes to the synthetic path (all registry
  tools require at least `path`, so it would have failed dispatch anyway).
- **SSE args sanitization**: `stream.ToolCallEvent` embeds args as
  `json.RawMessage`; embedding invalid JSON would corrupt the enclosing SSE
  frame, so the truncated call's event carries `{}`. The pair mirrors the
  `bad_args` visibility precedent (frontend renders the failure from the
  tool_result content).
- After scaffolding, the continuation re-requests with the same `round`;
  the model typically re-issues the truncated call completely (fresh
  `tool_call` id, normal dispatch).

### D6 — Expansion arithmetic

Attempt N (0-based) sends `MaxTokens = cfg.MaxTokens + N × expand_step`.
Additive on the per-agent base — the agent's configured `max_tokens` stays
the source of the first-attempt budget (model-effort-v2 keeps `max_tokens`
per-agent; the expansion is turn-local, never written back to config or
agent state). The ceiling is implicitly `base + max_retries × step` — no
separate cap knob. For thinking-family entries this maps to
`max_completion_tokens` in the client unchanged (the helper only touches
`req.MaxTokens`).

### D7 — Retry bound and exhaustion

`max_retries` counts **continuations** (3 → 4 total attempts). Continuation
attempts do not consume `max_rounds` — they happen inside one round
iteration. If the final attempt still ends `length`: emit
`agent_error` (`Meta.error_code = "length_exhausted"`, message noting
attempts and final budget) + `agent_end`, return an error so the
orchestrator's `done` carries `error`, with `finalContent` = accumulated
content (continuation never discards, so partial output is preserved).
Mirrors the `round_cap_exhausted` precedent. When the feature is disabled,
byte-for-byte current behavior (silent terminal + existing WARN in
`shouldDispatchToolCalls`).

### D8 — Usage accounting

All attempts — including length-cut ones — fold into `total` and
`by_agent` (real spend; `llm_raw_log` already records each attempt as its
own `call_id` with the expanded request params, free post-mortem).
`observeRoundContext` (→ `usage.meta.context_tokens` →
`turn_usage.context_tokens`) records **only the final attempt's** usage —
it is the authoritative size of the next request; summing attempts would
inflate it. The continuation pipeline does not charge the sub-agent
`retryBudget` (that budget guards transient-error retries with a different
policy owner; the continuation burn is bounded by D6/D7 arithmetic).

### D9 — Config surface

```yaml
openai:
  length_continue:
    expand_step: 8192   # additive per-attempt increment
    max_retries: 3      # continuations before length_exhausted
```

Global (not per-agent): truncation is a gateway/model trait. Zero/unset
block = disabled. Once any field is non-zero, a missing sibling defaults
(`expand_step` 8192, `max_retries` 3). Negative or non-integer values fail
config load (same load-time rejection convention as
`stream_idle_timeout`). Fields are plain ints — no yaml.v3 fraction
truncation concern, but validation mirrors the catalog's shadow-check style
where cheap.

### D10 — Write-budget guidance: computed at tools[] render time

The steering sentence is appended to `xizhi_write_file` /
`xizhi_modify_file` descriptions when an agent's tools[] JSON is built
(`internal/agent/tools.go` — `buildRegularToolsJSON` /
`buildConfuciusToolsJSON` path), with the budget
`floor(cfg.MaxTokens × 0.7)`:

- Why not in the static `ToolSpec.Description`: `tool.Registry` is
  process-wide; `max_tokens` is per-agent — the same spec renders for
  agents with different budgets. The static description keeps the
  *pattern* advice (chunk large content, multiple smaller writes); the
  injected sentence carries the computed number ("keep each single write
  under ~N tokens …").
- 70% leaves headroom for the args JSON escaping inflation, any prose
  preamble, and parallel calls sharing one output budget.
- Applies only to agents whose `cfg.Tools` lists the two write-family
  tools; read-only agents are untouched.

### D11 — `xizhi_write_file` mode parameter

`mode` joins the args schema, enum `"write" | "append"`, **default
`"write"` = today's create-or-overwrite** (byte-for-byte behavior for
existing calls and prompts). `"append"`: create the file (and parents) if
missing, else append to the end. Result gains `appended` (bytes written
this call); `size` remains the new total so the model can verify growth.
Path validation (`validatePath`), permissions, and the result envelope are
unchanged. `xizhi_modify_file` needs no mode — its per-call budget guidance
(D10) steers large `new_content` to chunked appends via write-append.

## Risks / Trade-offs

- [glm may reject invalid-JSON `arguments` echoed back in the assistant
  message (B-call-1)] → verify against the gateway in the first deploy;
  documented fallback is to strip the truncated calls' args to `""` while
  keeping the calls + synthetic results (wire shape identical, model loses
  sight of its partial args). Tasks include an integration-style test with
  a scripted fake that asserts the round-trip shape we send.
- [Model repeats already-written content when continuing] → instruction
  wording ("do not repeat"); worst case is a cosmetic seam inside one
  merged message row. Accepted.
- [Models cannot count tokens; the 70% guidance is advisory] → it steers
  magnitude, not precision; the runtime continuation is the backstop.
- [Worst-case cost: 4 attempts × expanded budgets] → bounded by
  `max_retries × expand_step`; operator-tunable, observable via
  `llm_raw_log` and `turn_usage`.
- [Context-shape drift: scaffolding (instruction, synthetic results) lives
  in `round` only — no events, not persisted] → after the turn, reload
  reconstructs an equivalent-but-different-shaped context (e.g. consecutive
  assistant rows). Content-equivalent; same accepted class as
  `wrapUpInstruction` and compaction framing. No action.
- [Wrap-up round returning `tool_calls` after continuation] → existing
  preamble-rejection logic (`runWrapUpRound` returns "" →
  `round_cap_exhausted`) still applies; continuation runs first, rejection
  only on the final attempt's shape.

## Migration Plan

Config-only opt-in; no migration, no deploy order. Rollback = unset
`openai.length_continue` (helper short-circuits to a single attempt,
byte-for-byte prior loop behavior). The `mode` parameter is additive with a
backward-compatible default; old models/prompts that never send `mode` are
unaffected.

## Open Questions

None blocking. Settled during exploration: trigger scope (length only),
continuation vs regeneration (continue), B-call-1 variant, 3 continuations,
global config, 70% computed budget, `append` mode in scope.
