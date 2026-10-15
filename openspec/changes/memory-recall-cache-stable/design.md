## Context

Today's recall path (`internal/handler/message_stream.go:409-427`) appends the rendered `<user-memories>` frame as an ephemeral user-role message after the recovered history and before the new user message. The frame is never persisted, so on the next turn the slot it occupied carries the previous user message instead: the prompt prefix diverges there every turn. Each recall block is therefore billed once at full input price and never cache-reused, and the previous user/assistant exchange is re-billed every turn. With cached input commonly ~10x cheaper than uncached and agent prompts dominated by prefill, this is a recurring, silent cost multiplier in every memory-enabled multi-turn session.

The persistence/reconstruction machinery needed to fix this already exists: tool-call pairs are persisted (`tool_calls` rows + placeholder message rows), replayed deterministically by `MessagesToAgentMessagesIndexed`, handled by compaction stitching, excluded from memory capture (capture sources user + assistant text only), and rendered by the UI as tool cards. The industry-converged pattern (Claude memory tool, Letta archival memory) is exactly this shape: retrieval results become part of an append-only transcript.

## Goals / Non-Goals

**Goals:**

- Turn-over-turn payloads that are strictly append-only between compactions, so the LLM prefix cache covers all prior turns.
- Keep the existing automatic per-turn recall semantics (query = new message; OpenViking search unchanged).
- Eliminate repeated injection of the same memory entry within a session (incremental frames).
- No new persistence concepts: reuse the existing tool-call/message rows, reconstruction, compaction, and capture paths.
- Restart-safe dedupe state derived from durable rows, not a separate cache to keep consistent.

**Non-Goals:**

- A model-invoked memory search tool (true JIT retrieval). The synthetic-pair transcript shape keeps that migration open, but the model does not decide when to recall in this change.
- Cross-session or user-scoped dedupe — a new session starts with an empty injected set on purpose.
- Changes to OpenViking query/rendering parameters (`recall_limit`, `recall_token_budget`, sorting, Abstract preference) beyond filtering already-injected entries.
- Provider-side cache-breakpoint control (cache_control headers); append-only payloads are the portable fix.

## Decisions

### D1: Synthetic tool pair, not a marker row or a system-prompt digest

The recall injection becomes an `assistant` message carrying one synthetic tool call (`recall_memories`) followed by a `tool` result message containing the rendered frame, inserted immediately before the new user message and persisted through the normal send-time write path.

Alternatives rejected:

- **Marker user row** (`event_type=memory`): invents a new row shape, needs marker-based capture exclusion, UI special-casing, and adjacent-user-message semantics; every layer must learn the marker. The tool pair is already first-class everywhere.
- **Byte-stable digest pinned after the system prompt**: cache-optimal but forfeits per-turn relevance — recall is query-driven by design.
- **Replay-only (persist the old user-message shape)**: cache-correct but keeps the worst display/capture/compaction story and the awkward adjacent-user pair.

Consequences: the model sees memories inside a `tool` result (standard, well-understood shape); the frame keeps its `<user-memories>` tags inside the tool content so the "background context, don't recite" instruction stays attached.

### D2: Arguments carry identity, not bulk

Tool-call arguments are a small deterministic JSON object: `{"query": "<≤160-char preview>", "entries": ["<uri>", ...]}`. `entries` is the machine-readable injected-set for dedupe derivation; the capped query preview exists for debuggability and the UI card. The full query is never duplicated — the real user message follows immediately.

### D3: Dedupe key = memory URI, content-hash fallback

`openviking.MatchedContext.URI` is the stable entry identity. Hits with an empty URI dedupe on `sha256` of the rendered line (score-stripped text), so abstract edits don't force re-injection but content changes do. The injected set is derived at turn start by collecting `entries` from reconstructed `recall_memories` calls in the recovered history — no new Redis key, and the set survives restarts with the same durability as the history itself.

### D4: Incremental frame; empty means no row at all

`Recall` filters hits against the session's injected set before rendering; only new entries render, still bounded by `recall_token_budget` and the per-entry cap. Zero new entries → no tool pair is built, persisted, or sent; the payload is pure history extension. On the first turn of a session the set is empty and the frame is today's full block.

### D5: Ordering and identity

Recall still completes before the send-time user-row write (the existing history-read invariant). The pair is persisted immediately before the user row of the same turn (same batch when the send-time write batches, otherwise the preceding write), so reconstruction ordering is pair → user row → assistant rows. The `ToolCallID` and placeholder row identity derive deterministically from the session id and the turn's user-row client message id, making client retries idempotent.

### D6: Compaction interaction

The pair is an ordinary tool pair to compaction: it lands in the summarized middle or retained tail per the existing rules. When old recall calls are summarized away their URIs leave the derived set, so a still-relevant memory may be re-injected after compaction — accepted and bounded (the prefix is broken at the compaction boundary anyway, and re-injection is capped by the per-turn budget).

## Risks / Trade-offs

- [Gateway rejects a historical tool call whose name is not in the current tools list] → Reconstruction already replays historical calls for tools no longer registered; verify against the concrete gateway in an integration test. If rejected, register a stable no-advertisement `recall_memories` spec (a fixed tool definition is cache-stable).
- [Context grows with each distinct memory] → Incremental-only frames bound growth to the distinct-entry set; compaction remains the overflow valve; `recall_token_budget` still caps each frame.
- [Post-compaction re-injection repeats a memory the summary already absorbed] → Accepted: bounded, and arguably correct once the verbatim frame is gone.
- [URI-less hits with drifting abstracts defeat dedupe] → Content-hash fallback; URI-less hits are expected to be rare.
- [Rollback leaves tool pairs in history] → Harmless by construction: the pair replays as an ordinary transcript row whether or not the new injection path is active.

## Migration Plan

1. Land reconstruction + persistence of the pair behind the existing `memory.enabled` flag (no new config).
2. Existing sessions transition naturally: their next memory-enabled turn starts with an empty derived set, injects one full incremental frame, and is append-only from there.
3. Rollback = revert the release; persisted pairs remain valid history.

## Open Questions

- UI copy for the `recall_memories` card (e.g. "Recalled N memories") — resolve during frontend wiring.
- Whether the query preview belongs in arguments at all if the UI can source the adjacent user row; default is to keep the capped preview for standalone debuggability.
