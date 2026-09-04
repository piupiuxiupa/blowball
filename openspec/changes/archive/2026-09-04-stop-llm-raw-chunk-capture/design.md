## Context

`OpenAIClient.StreamChat` currently emits a raw-capture row for the request, every accepted SSE frame, and the terminal response/error. The implementation has no capture configuration: the sink is always attached in agent/all roles, and per-call chunk bytes are bounded at 8MB. Historical `kind=chunk` rows remain valid data in existing deployments.

## Goals / Non-Goals

**Goals:**

- Stop growing `llm_raw_log` with individual SSE-frame rows.
- Preserve complete request payloads and stitched response/error payloads for post-mortem analysis.
- Keep the existing schema and Redis/MySQL write path compatible with historical chunk rows.

**Non-Goals:**

- No configuration toggle; new capture behavior is always request + terminal row.
- No automatic deletion, retention, or compaction of existing rows.
- No schema migration or index redesign.

## Decisions

- Remove the `captureFrame` call from the streaming loop rather than filtering `kind=chunk` in the sink. This avoids serializing frames, allocating capture records, and pushing them to Redis in the first place.
- Keep `model.RawKindChunk` and the `frame_index` column. They are still needed to interpret rows written by older deployments.
- Emit terminal rows at `frame_index=1`. With chunks disabled, calls now consistently produce request `0` followed by response/error `1`.
- Leave schema and flusher behavior unchanged. No migration is needed because `kind` and `frame_index` remain valid historical columns.

## Risks / Trade-offs

- New captures can no longer replay exact wire-level SSE framing or prove which partial frames arrived before a process crash → The stitched response preserves aggregated content/tool calls/usage, and the request plus terminal error still identify failed calls; deployments needing frame forensics can roll back this code change.
- Existing chunk rows continue to consume space → Operators may explicitly run a scoped `DELETE FROM llm_raw_log WHERE kind='chunk'` after backing up data; this change deliberately does not delete anything automatically.
