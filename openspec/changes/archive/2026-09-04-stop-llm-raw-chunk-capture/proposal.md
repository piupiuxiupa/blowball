## Why

`llm_raw_log` currently stores one row for every SSE chunk. Long streaming turns therefore generate hundreds of high-overhead rows per call, making the table large and difficult to search even though the stitched `kind=response` row already preserves the final aggregated content.

## What Changes

- Stop emitting new `kind=chunk` rows from `OpenAIClient.StreamChat`.
- Continue capturing `kind=request` and the terminal `kind=response` or `kind=error` row for every LLM call.
- Remove the per-call 8MB frame-capture budget because there are no new chunk rows to budget.
- Preserve the `chunk` kind, `frame_index` column, and write path so existing historical rows remain readable.
- Do not add an automatic cleanup job; operators may explicitly delete historical chunk rows if desired.

## Capabilities

### New Capabilities

- None.

### Modified Capabilities

- `llm-raw-capture`: Raw capture no longer requires per-SSE-frame chunk rows or frame truncation; it records request and terminal response/error payloads only.

## Impact

- Affects `internal/agent` raw-capture behavior and tests.
- Updates comments in the raw-log model and schema documentation where needed.
- No HTTP API, database schema, Redis buffer, flusher, or config changes.
