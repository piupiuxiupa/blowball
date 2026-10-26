## Context

The current compaction flow selects history by message count, summarizes the span between the first message and the retained tail, persists the checkpoint, and rewrites the model context. It cannot prove that the rewritten context fits, and its pressure measurement happens before the current round's tool results are sent. The context-request-budgeting capability closes the preflight and provider-error gap; the updated context-compaction capability makes the produced context genuinely bounded.

The model-facing protocol constrains the design: an assistant message containing `tool_calls` must be followed by one `role=tool` message per call. Large tool results therefore cannot simply be deleted from a retained tail while their calls remain. The design distinguishes a complete semantic invocation (tool name, exact arguments, call identity, outcome summary) from replay of the full result body.

The persistent transcript and message-history API are not changed. Compaction remains a model-context projection; the display path continues to expose every persisted row.

## Goals / Non-Goals

**Goals:**

- Make every eligible model request fit the selected model's input budget before it is sent, including tool results appended after the last measured round.
- Produce a compacted context that is validated against that budget before its record becomes authoritative.
- Preserve recent user messages, assistant prose, and assistant reasoning for continuity.
- Preserve valid tool-call/tool-result protocol pairs while replacing large result bodies with deterministic summaries and retrievable full-output references.
- Preserve a session-level account of earlier tool use: names, counts, statuses, important arguments, and concise outcomes.
- Recover once from a provider context-overflow error by force-compacting and rebuilding the request, rather than retrying the unchanged oversized request indefinitely.
- Make compaction outcomes diagnosable through durable pre/post size and strategy fields.

**Non-Goals:**

- Changing the persisted chat transcript or adding summary rows to the message-history API.
- Replacing the shared conservative estimator with a model-specific tokenizer.
- Guaranteeing success for an irreducibly oversized request. Unavoidable failure becomes an explicit uncompactable-context outcome.
- Making every retained result summary a fresh LLM call on every model request.
- Migrating old compaction records into a new authoritative form.

## Decisions

### D1: A request budget combines authoritative usage with a conservative delta

The mid-turn next-request estimate is:

```text
estimatedNextInput =
    lastPromptTokens
  + lastCompletionTokens
  + Estimate(added assistant/tool-retrieval messages)
  + fixed protocol overhead

requestHeadroom =
    maxContextTokens
  - maxCompletionTokens
  - safetyMargin
```

The estimator's current mixed-text and JSON bias is documented in
`internal/tokens/tokens.go`; JSON-dense tool envelopes need a conservative
multiplier rather than assuming all non-CJK text is four characters per token.
The first request of each turn uses the conservative estimator over the actual
rendered messages, system prompt, tool descriptors, and current user message.
This check runs after the per-turn agent is built, so it measures the real
request rather than the previous turn's stored usage.

Compaction begins at the lower of the existing 80% threshold and a configurable/deterministic preflight threshold derived from `requestHeadroom`. The output quota is reserved because a request can be rejected when input plus the requested completion budget exceeds the model window.

**Alternative rejected:** reserve only a fixed percentage. It behaves poorly across models whose completion quotas differ substantially and makes the existing completion quota irrelevant to admission.

### D2: The retained tail is selected by importance and budget, not a fixed count

The planner walks backward from the newest message while tracking a separate tail budget. It preserves:

1. recent user messages;
2. recent assistant prose and reasoning;
3. complete recent tool invocation units with summarized tool results;
4. older tool-invocation summaries in the checkpoint ledger.

It never cuts an assistant tool-call message away from all of its tool results, never splits a parallel result set, and never drops the synthetic recall-memory pairing. A complete unit either stays paired or moves wholly into the summarized/checkpoint material. Token size, not the count five, determines where the tail starts.

**Alternative rejected:** retain the last five messages then truncate individual tool results in place without re-planning. A few huge results can still exceed the window, while a small conversational tail needlessly discards useful continuity.

### D3: Retained tool results become deterministic summaries with stable identity

A retained paired tool result body is replaced by a stable text/JSON summary containing:

- tool name and original `tool_call_id`;
- success or error status and exit code when available;
- whether output was truncated;
- a short semantic or head/tail summary;
- original approximate size;
- a workspace-relative or persisted artifact reference when full output exists.

Known tool families use bounded deterministic summarizers:

- shell commands: exit status, relevant error excerpts, key command/path identifiers;
- file reads: path, selected modified regions or identifiers, size;
- searches/globs: counts and representative matches;
- web fetches: title/principal findings and relevant links;
- MCP calls: tool/operation, arguments, success/error and concise result;
- recall memory: preserve its frame and identifiers as a special synthetic unit.

Unknown tools use a bounded head/tail preview plus size, status, and a stable full-result reference. The summarizer must not invent facts and must preserve error strings, identifiers, and paths verbatim where they remain in the retained excerpt.

Summaries are computed when the compaction candidate is planned and then persisted in the compaction record or replayed deterministically from stored summaries. They are not freshly model-generated for every subsequent LLM round, preserving prefix stability and avoiding a second failure surface.

**Alternative rejected:** use an LLM to summarize every retained tool result on every request. It is expensive, nondeterministic, and can itself overflow before the agent can recover.

### D4: The checkpoint carries a bounded tool-activity ledger

Tool invocations that move outside the retained tail contribute to a structured checkpoint section such as:

```text
## Tool Activity
- <tool name>: <calls> calls, <successes> succeeded, <errors> failed
  - <important argument/identifier>: <short outcome>
```

The ledger retains exact tool names, counts, important parameters, failure strings, and outcome references. Its own size is bounded: it condenses older entries before allowing the checkpoint to grow without limit. The final checkpoint remains a single structured summary, not a stack of per-round summaries.

### D5: Compaction candidates are validated before publication

Planning produces a candidate context and its estimated request size. Before saving the authoritative record, the candidate must satisfy all of:

```text
estimated request size <= request input budget
all retained assistant tool_calls have matching tool results
the first-message anchor is present
the framed checkpoint is present once
the retained tail contains only post-boundary messages
```

If the first attempt does not fit, apply bounded deterministic reductions in order: reduce tail retention, condense retained result summaries, externalize a result that has not yet been externalized, condense the tool ledger. If the unavoidable first-message anchor plus a minimal checkpoint and protocol units still do not fit, return an `uncompactable` outcome with a diagnostic reason; do not save a record whose context is still oversized.

The initial first-message anchor may itself be bounded only when it exceeds the reserved anchor budget. Preserve a head/tail excerpt plus size/retrieval reference in the checkpoint, while retaining the original persisted row unchanged. This changes the existing “first message verbatim under all circumstances” guarantee and requires a deliberate spec modification.

### D6: Summary generation is chunked to fit the summary model

The summarizer partitions the shadowed middle plus prior checkpoint into budgeted chunks. It processes them serially as `prior/accumulated summary + chunk → consolidated summary`, leaving room for the fixed system prompt and output quota. This replaces the current one-shot transcript that can overflow before compaction completes.

If a chunk is still oversized because of a single exceptional payload, externalize/summarize that payload first; the summary request then references it instead of embedding the full body. Chunking is ordered so the final summary cannot silently omit the preserved first-message anchor or the newer span.

### D7: Provider context-overflow is a typed, separately handled failure

The OpenAI adapter classifies a provider context-overflow error into a typed sentinel while retaining the raw status/body through the existing error capture. It is not retried by the transient retry loop.

The agent-level recovery path receives an error before it has a valid next-round usage observation. It triggers one forced compaction attempt against the current turn snapshot and reconstructs the request. A second overflow terminates with an actionable uncompactable-context error and is recorded in logs/metrics; it does not re-enter the unchanged request or repeatedly summarize on every turn.

**Alternative rejected:** treat all 400s as context overflow. That would turn malformed requests or unsupported parameters into expensive forced compactions and could mask contract errors.

### D8: Observability is stored with the compaction and usage data

Add backward-compatible metadata to compaction records:

- trigger kind;
- measurement basis: provider-observed versus estimated;
- pre-compaction request estimate;
- target input budget and model window;
- post-compaction estimate;
- retained message/turn count;
- summary strategy and result;
- tool summary/activity counts.

The migration adds nullable/defaulted columns or a compact JSON observation
column; old records remain valid. Existing `turn_usage.context_tokens` remains
the last provider-observed round size for observability, but is not used as
proof that a newly constructed request fits.

## Risks / Trade-offs

- **Loss of tool-result detail** → Preserve exact call identity, key outcomes, failure text, sizes, and retrievable full-output paths; keep recent results more detailed than older ledger entries.
- **Heuristic underestimation for dense JSON** → Use a conservative envelope multiplier, keep a safety margin, and validate against provider-observed usage when available.
- **Wrong classification of a generic 400** → Require specific provider overflow signals and keep the original error body; non-overflow errors remain semantic failures.
- **Compaction thrash after a real overflow** → Limit forced recovery to one attempt per turn/request and persist the resulting record so later turns start from the compacted context.
- **Chunked summaries increase latency and cost** → Bound chunk size/count and short-circuit when the existing one-shot input fits.
- **Irreducibly large first message or single required payload** → Return an actionable uncompactable-context diagnostic rather than looping or silently mutating the transcript.
- **Prefix-cache invalidation** → Compute and persist summaries at compaction time; avoid nondeterministic per-request re-summarization.

## Migration Plan

1. Introduce the request-budget calculator and typed provider-overflow classification behind unit tests; no persistence change is required for this stage.
2. Add budgeted candidate planning and candidate validation while still writing existing compaction record fields and preserving legacy records.
3. Add deterministic tool-result summaries and the activity ledger, then enable candidate-based compaction for new records.
4. Add optional observation fields with backwards-compatible defaults. Existing rows continue to be read; no backfill is required.
5. Roll back by disabling forced overflow recovery or restoring the old compaction planner. Persisted messages, tool calls, and old compaction records remain usable.

Deployment must also verify that each catalog entry's `max_context_tokens` is the actual gateway model window. The current configuration of 1,000,000 for several entries is only safe if the gateway truly supplies that window; otherwise the budget planner would trigger too late.

## Open Questions

- Should the safety margin be a fixed small percentage, a catalog field, or a global configuration value? The implementation can begin with a documented fixed conservative margin and expose an operator knob only if gateway variance requires it.
- Should full externalized tool outputs live under the existing per-user workspace `tmp/` area or a dedicated retention area with explicit expiry? Reusing the workspace keeps retrieval local, but needs a policy for cleanup and path visibility.
- Which tool families should receive bespoke semantic summaries first, and which can remain on the deterministic head/tail fallback for the initial delivery?
- For an oversized first user message, is a head/tail excerpt in the checkpoint sufficient, or should the planner require the model to reread the original row through a retrieval tool before continuing?
