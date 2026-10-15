## 1. Memory service: entry identity and incremental rendering

- [x] 1.1 Change `memory.Service.Recall` to return structured hits (URI, rendered line, score) alongside the frame, keyed for dedupe (URI primary, sha256 of rendered text fallback)
- [x] 1.2 Add dedupe filtering to rendering: exclude hits whose key is in the session's injected set, keep score-sort / Abstract preference / per-entry cap / aggregate budget / top-scored-new-entry guarantee
- [x] 1.3 Unit tests in `internal/memory/`: incremental frame contents, URI-less fallback, budget with new-only entries, empty-when-nothing-new

## 2. Handler: synthetic pair build, persistence, and injected-set derivation

- [x] 2.1 Derive the session's injected key set from `recall_memories` calls in the recovered history (arguments `entries`) at turn start
- [x] 2.2 Replace the ephemeral user-message append with the synthetic `assistant` tool-call + `tool` result pair (deterministic ids from session id + user-row client msg id; capped query preview in arguments)
- [x] 2.3 Persist the pair before the turn's user row through the send-time write path; skip entirely when the incremental frame is empty or recall failed
- [x] 2.4 Verify `TurnCapture` still sources only user/assistant text with the pair present (no marker logic)
- [x] 2.5 Handler tests: pair ordering vs user row, retry idempotency, no-new-entries shape, recall-failure degradation

## 3. Reconstruction, compaction, and gateway verification

- [x] 3.1 Verify `MessagesToAgentMessagesIndexed` replays the persisted pair byte-identically (id, arguments, result content) and add a regression test for append-only third-turn payloads
- [x] 3.2 Verify compaction stitch handles the pair in middle/tail positions and add a test
- [x] 3.3 Integration test against the real gateway: historical `recall_memories` call with the name absent from the current tools list is accepted; if rejected, register a stable no-advertisement tool spec and re-test cache stability
- [x] 3.4 Integration test: Redis-expired and restart-recovered histories still derive the injected set from durable rows

## 4. UI and end-to-end validation

- [x] 4.1 Confirm the transcript renders the pair as an ordinary tool-call card (add UI copy, e.g. "Recalled N memories"); hide nothing behind new row shapes
- [x] 4.2 End-to-end multi-turn check: compare consecutive sent payloads and confirm byte-identical prefix through the previous turn (uncached suffix = prior turn's new rows + current injection + user message only)
- [x] 4.3 Update `openspec/specs/cross-session-memory/spec.md` expectations in any dependent docs/tests; run `make test`
