## Why

Cross-session memory recall injects a fresh, query-dependent block before the latest user message every turn, and the block is ephemeral: it is never persisted and never replayed. Because the slot the block occupied in turn N is occupied by the persisted user message in turn N+1, the LLM prompt prefix diverges at that point every turn — each recall block is paid at full input price exactly once, the previous user/assistant exchange is re-billed every turn, and the prefix cache only ever covers history older than the last turn. With agent input dominated by prefill (and cached input commonly ~10x cheaper than uncached), this silently multiplies cost in every memory-enabled multi-turn session.

The industry-converged fix is to make the transcript append-only: retrieval results enter the conversation as persisted tool calls/results that later turns replay verbatim (the Claude memory-tool / Letta archival-memory pattern).

## What Changes

- Recall injection moves from an ephemeral user-role message to a **synthetic `recall_memories` tool-call/tool-result pair** inserted immediately before the new user message, persisted through the existing tool-call persistence path.
- History reconstruction replays the pair byte-identically, so payloads across turns become strictly append-only between compactions and the prefix cache covers all prior turns.
- Per-turn frames are **incremental within a session**: entries already injected earlier in the session (keyed by memory URI, with a content-hash fallback for URI-less hits) are omitted; a turn with no new entries injects nothing and the payload is pure history extension.
- Memory capture is unchanged in effect: `TurnCapture` continues to source only the user/assistant pair, and tool rows are naturally excluded — no marker-based exclusion and no memory-about-memory feedback loop.
- The recall block remains visible in the transcript as a tool-call card (existing UI shape for tool rows).
- Compaction treats the pair exactly like any other tool pair (summarized into the checkpoint middle when triggered).

## Capabilities

### New Capabilities

- `memory-recall-replay`: Defines the persisted, append-only replay semantics of turn-start memory recall — synthetic tool-pair injection, session-scoped incremental dedupe, byte-identical history reconstruction, and interaction with compaction.

### Modified Capabilities

- `cross-session-memory`: The "Turn-start recall injects an ephemeral block" requirement changes — the block is no longer ephemeral. It is persisted as a tool pair, replayed by history recovery, and deduplicated within the session; capture exclusion now follows from sourcing (user/assistant only) rather than ephemerality.

## Impact

- `internal/handler/message_stream.go` — recall injection site (replace ephemeral user append with synthetic tool-pair construction + persistence bookkeeping).
- `internal/memory/` — `Recall` gains entry-identity output (URI/content hash) so the handler can dedupe within the session; rendering of the incremental frame.
- `internal/handler/message_reconstruct.go` / persistence layers — replay of the persisted tool pair (mostly existing machinery); send-time ordering with the user row.
- `internal/service` compaction + `internal/handler/compaction_stitch.go` — verify tool-pair handling covers the synthetic call.
- Frontend transcript rendering — recall appears as a tool call card.
- `openspec/specs/cross-session-memory/spec.md` — ephemeral requirement replaced.
- No HTTP API, config, or schema contract changes are expected beyond the message/tool-call rows the existing persistence path already writes.
