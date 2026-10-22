## 1. Characterization and Regression Tests

- [x] 1.1 Add a stream-event test proving `json.RawMessage` tool arguments retain insignificant whitespace and byte order instead of being compacted.
- [x] 1.2 Add round-persistence tests for narrated tool rounds, parallel calls, mixed `update_plan` calls, and consecutive tool-only rounds.
- [x] 1.3 Add a reconstruction test that serializes the live request messages and recovered request messages through the same conversion path and compares the completed prior prefix byte-for-byte.
- [x] 1.4 Add mixed legacy-event/new-final-row coverage without weakening the new provider-native replay shape.

## 2. Stable Tool-Call Event Generation

- [x] 2.1 Update tool-call stream-event construction so already-raw JSON arguments are retained verbatim while non-raw argument values remain deterministically marshalable.
- [x] 2.2 Separate tool-call event emission from tool execution so every call event is emitted synchronously in LLM response order before parallel dispatch begins.
- [x] 2.3 Preserve `update_plan` result semantics while including its placeholder in the same response-order call-event pass.
- [x] 2.4 Apply the same deterministic event ordering to root and generic sub-agent dispatch paths where their shared tool-loop behavior is affected.

## 3. Round-Boundary Rendering

- [x] 3.1 Update round rendering to close a completed round when a new tool-call event follows a tool result.
- [x] 3.2 Verify all calls in one response are emitted before any result so the new boundary rule cannot split a parallel round.
- [x] 3.3 Keep deterministic final-row and placeholder ids stable across mid-turn, compaction-triggered, and terminal renders.

## 4. Deterministic Reconstruction

- [x] 4.1 Refactor final-row reconstruction to buffer an assistant final row and join adjacent same-round tool placeholders into one assistant message carrying content, reasoning, and tool calls.
- [x] 4.2 Emit tool result messages and order `tool_calls` by the persisted response-order placeholders, pairing canonical results by call id.
- [x] 4.3 Preserve the legacy token/reasoning/tool-event adapter and its existing handling of unpaired or pending calls.
- [x] 4.4 Update row attribution for merged assistant/tool units so compaction boundaries use the full source-row range.

## 5. Cross-Capability Regressions

- [x] 5.1 Add a compaction-boundary test that retains an assistant tool-call message and its tool results as one unit in the retained tail.
- [x] 5.2 Add a synthetic `recall_memories` replay test confirming ordinary tool-history fixes do not alter the recall pair's append-only prefix behavior.
- [x] 5.3 Cover recovery from both Redis-backed final rows and MySQL final rows with identical reconstructed bytes.

## 6. Validation and Cleanup

- [x] 6.1 Run the focused handler, agent, stream, and persistence tests.
- [x] 6.2 Run `go test ./...` with race detection.
- [x] 6.3 Confirm no HTTP contract changed and no `api/openapi.yaml` update is required.
- [x] 6.4 Run OpenSpec validation and review the change artifacts for consistency.
