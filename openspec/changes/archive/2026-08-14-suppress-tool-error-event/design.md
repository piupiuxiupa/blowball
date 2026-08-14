## Context

Today a registry-tool failure is signalled on **two independent channels**:

1. **Model-facing (in-band):** the `role="tool"` message body is rendered by the shared `renderToolResult` into `{"status":1,"error":"<msg>"}` — the `status` envelope. This is what the model sees and reacts to.
2. **Frontend-facing (out-of-band):** an `agent_error` SSE event with `Meta.code=tool_error` and `Content=<msg>`.

The dual emission is mandated by the `tool-result-envelope` requirement *"Envelope coexists with the agent_error stream event"*, and lives in three near-identical dispatch sites: `Confucius.dispatchRegistryTool`, `Chongzhi.dispatchOneRegistryTool`, `Liang.dispatchOneRegistryTool`. Each calls `streamAgentError(hub, ctx, name, err.Error(), "tool_error")` inside the `toolRegistry.Call` error branch, then unconditionally returns `toolResult{content: renderToolResult(out, err), isError: err != nil}`.

Investigation of the frontend (`blowball-frontend`, sibling repo) shows it never uses `agent_error` as the *source* of tool-error rendering:

- `tool-call-bubble.tsx` derives `isError` from `tool_result`'s `status` field (`isStatusError(status)` → red bubble + warning icon).
- `agent_error` instead calls `setSegmentStatus(..., 'error')`, and `ui-store.ts` latches `isError: status === 'error' ? true : seg.isError` — so a later `agent_end` (status `'idle'`) does **not** clear it. A tool failure therefore turns the whole agent segment red for the rest of the stream, even after a clean recovery.
- On history reload, `message-list.tsx` maps a persisted `EventTypeAgentError` row to `current.content += "\n\n[错误] <msg>"` + `current.isError = true`, double-displaying the tool error (once appended to the agent text, once as the red tool bubble).

So the `tool_error` `agent_error` is both redundant (the `status` field already carries it to both model and UI) and misleading (overstates a recoverable tool hiccup as an agent failure).

## Goals / Non-Goals

**Goals:**
- Make the `status` envelope the single, authoritative channel for registry-tool failures.
- Remove the misleading whole-agent-segment error marking and the `[错误]` text-append on reload for tool failures.
- Keep the model fully informed of tool failures (envelope unchanged) and the UI's red tool bubble intact (driven by `status`).

**Non-Goals:**
- Touching any `agent_error` code other than `tool_error` (`unknown_tool`, `bad_args`, `llm_error`, `retry`, `round_cap_exhausted` stay).
- Changing `renderToolResult`, the envelope shape, or the `role="tool"` message body.
- Migrating already-persisted historical `EventTypeAgentError` rows — old sessions keep their existing reload rendering; only new turns change.
- Changing the `unknown_tool` (nil-registry) path, which returns a raw error string rather than the envelope — that is a separate, pre-existing inconsistency, out of scope here.

## Decisions

### Decision 1: Remove only the `tool_error` emission; keep everything else
Delete the single `streamAgentError(..., "tool_error")` call in the `toolRegistry.Call` error branch of all three dispatch sites. Leave `renderToolResult(out, err)` exactly as-is — it still produces the envelope whether or not the event is emitted, because it runs unconditionally after the error check.

**Alternatives considered:**
- *Keep emitting `agent_error`, make the frontend ignore `code=tool_error`.* Rejected: offloads contract knowledge to every frontend consumer, still persists the misleading row, and leaves the server emitting an event nobody should use.
- *Suppress `agent_error` for all tool-adjacent codes including `unknown_tool`/`bad_args`.* Rejected: those represent the agent misusing a tool or dispatching with malformed args — genuine agent-level misbehavior that deserves an `agent_error`. Only routine tool *execution* failures (`tool_error`) are the recoverable, status-envelope-covered case.

### Decision 2: No persistence or schema change
The change is purely an SSE-stream emission change. Fewer `EventTypeAgentError` rows are written for tool-failure turns going forward; the tool failure remains durably visible via the persisted `tool_result` row (`status:1`). No migration, no new column, no backfill. Historical rows are left untouched.

### Decision 3: Document as a BREAKING SSE-contract change
Tool failures no longer produce an `agent_error` event. This is flagged BREAKING so any non-bundled consumer is aware, even though the bundled frontend needs no code change (it is already `status`-driven). `api/openapi.yaml` and CLAUDE.md prose stating tool failures emit `agent_error` will be updated to match.

## Risks / Trade-offs

- **[Third-party SSE consumers lose the tool-error event]** A consumer that branches on `agent_error` to detect tool errors would no longer be notified. → *Mitigation:* flagged BREAKING; the `status` field in `tool_result` is the stable, documented contract. The bundled frontend is unaffected.
- **[Severe tool errors become less "loud"]** A tool timeout or registry misconfiguration no longer fires `agent_error`. → *Mitigation:* it is still server-logged, still shown as a red tool bubble via `status:1`, and still fed to the model (which can report it in its reply). `agent_error` was never the right severity for a recoverable tool failure anyway.
- **[Historical reload rendering is inconsistent]** Old sessions still show `[错误] <msg>` on reload for past tool failures; new sessions won't. → *Mitigation:* acceptable — historical data is read-only; no migration is worth the cost. The inconsistency fades as old sessions age out.

## Migration Plan

1. Deploy the backend change (three single-line removals + comment updates).
2. Update `api/openapi.yaml` and CLAUDE.md prose, then re-sync OpenAPI into `blowball-frontend` (`npm run generate-api`) — no FE logic change.
3. Update any unit/integration test that asserts a tool failure emits an `agent_error` (code `tool_error`) to instead assert the envelope-only behavior.

**Rollback:** revert the three emission lines (and re-add the now-updated tests). No data rollback is needed.
