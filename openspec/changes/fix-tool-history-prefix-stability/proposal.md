## Why

Persisted tool-call history does not currently replay the live agent-round wire format byte-for-byte. A narrated assistant round can be reconstructed as separate content and tool-call messages, tool argument whitespace can be normalized, and parallel calls can be persisted in dispatch-completion order; these differences break the provider prefix cache at turn boundaries and after durable recovery. The existing message-final-persistence contract already promises byte-stable reconstruction, so this is a correctness gap rather than a new behavior.

## What Changes

- Make the live LLM response shape canonical for replay: an assistant round containing content, reasoning, and tool calls reconstructs as one assistant message followed by its tool results.
- Preserve the provider's original tool-call argument JSON bytes across stream-event persistence and reconstruction, rather than compacting or re-rendering them only on replay.
- Persist and replay tool calls in the order emitted by the LLM response, independent of parallel execution or goroutine scheduling.
- Keep consecutive tool-only LLM rounds separate when rendering round-final persistence batches.
- Add live-versus-recovered request-prefix regression coverage, including mixed content/reasoning/tool-call rounds, parallel calls, consecutive tool-only rounds, legacy/new history, compaction boundaries, and synthetic memory-recall pairs.
- Do not change memory-recall retrieval, rendering, dedupe, ordering, or HTTP contracts in this change.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `message-final-persistence`: Strengthen deterministic cross-table reconstruction so a completed LLM round retains its assistant content/reasoning/tool-call combination, original argument bytes, and response-order tool sequence across live use, persistence, and recovery.

## Impact

- Affects agent tool dispatch and stream-event emission in `internal/agent`.
- Affects round-final persistence in `internal/handler`.
- Affects message/tool-call reconstruction and compaction row attribution in `internal/handler`.
- Requires focused unit tests around live requests, round persistence, and recovered requests.
- No client-visible HTTP API change is expected, so `api/openapi.yaml` is not expected to change.
- The initial design avoids database migrations; historical repair is out of scope unless explicitly needed to validate the new invariant.
