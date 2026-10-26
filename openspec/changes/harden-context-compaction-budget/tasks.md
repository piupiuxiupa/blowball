## 1. Request Budget Foundations

- [x] 1.1 Extend the token utility with a conservative rendered-request estimator for mixed prose and JSON/tool envelopes; add table tests proving JSON-dense input is not under-estimated silently.
- [x] 1.2 Add a request-budget calculator that derives effective input budget from model window, completion quota, and safety margin; cover positive, zero, and negative remaining budgets.
- [x] 1.3 Add pre-request estimation based on latest provider usage plus newly appended messages, with whole-request fallback when no usage exists; test a small measured round followed by a large tool result.
- [x] 1.4 Identify the model/tool-descriptor overhead sources and include them in whole-request estimation without duplicating them in provider-usage deltas.

## 2. Provider Overflow Classification

- [x] 2.1 Introduce a typed context-overflow sentinel at the OpenAI client boundary and classify provider error bodies/statuses conservatively; add tests for known overflow, malformed-request 400, 429, and 5xx.
- [x] 2.2 Ensure context overflow bypasses the ordinary transient retry path while preserving raw capture of the provider response.
- [x] 2.3 Expose the typed overflow and latest request estimate to the agent loop without leaking provider-specific error parsing below the client boundary.

## 3. Budgeted Compaction Planning

- [x] 3.1 Replace fixed tail-count selection with a planner that walks backward by token budget and preserves recent user prose, assistant prose, and assistant reasoning first.
- [x] 3.2 Preserve complete tool invocation units, parallel result sets, and recall-memory pairs; add tests for a boundary inside a pair and for a unit whose retained results exceed budget.
- [x] 3.3 Implement bounded first-message anchor handling: verbatim when it fits, bounded excerpt plus size/reference when it does not; verify persisted transcript rows remain unchanged.
- [x] 3.4 Define the candidate context shape and validation checks for input budget, complete pairing, one framed checkpoint, anchor presence, and post-boundary tail.

## 4. Tool Result Summaries and Activity Ledger

- [x] 4.1 Implement a deterministic tool-result summary schema containing name, call ID, status, exit code, truncation state, outcome, size, and retrieval reference.
- [x] 4.2 Add bounded head/tail fallback summarization for unknown tools, with tests for UTF-8 boundaries and preservation of exact error strings and identifiers.
- [x] 4.3 Add type-aware bounded summarizers for shell commands, file reads, searches, web fetches, MCP calls, and recall-memory pairs.
- [x] 4.4 Persist or externalize large full tool outputs and return stable workspace/persisted references; test a failed write by degrading to a bounded preview rather than returning full output.
- [x] 4.5 Build the checkpoint tool-activity ledger with tool names, counts, success/failure counts, important arguments, and concise outcomes.
- [x] 4.6 Merge prior activity into one bounded ledger across re-compactions and condense older entries so the checkpoint cannot grow without bound.

## 5. Compaction Fit and Chunked Summaries

- [x] 5.1 Validate the initial compaction candidate before writing an authoritative record; reject or reduce oversized candidates without persisting a success state.
- [x] 5.2 Implement bounded deterministic reduction order: tail reduction, result-summary condensing, externalization, ledger condensing.
- [x] 5.3 Return an actionable uncompactable-context result when the irreducible anchor/checkpoint/protocol unit remains over budget.
- [x] 5.4 Chunk the shadowed transcript plus prior checkpoint for summary generation, reserving system prompt and completion quota; use the existing one-shot path when the input fits.
- [x] 5.5 Handle an exceptional oversized transcript payload by summary or externalization before chunking, and add a test where the current one-shot summary would overflow.
- [x] 5.6 Recompute and validate the final candidate after the summary is generated; only then make the record authoritative.

## 6. Request-Time Compaction and Recovery

- [x] 6.1 Replace the round hook's previous-usage-only comparison with the pre-request estimate and effective safe threshold.
- [x] 6.2 Add the same preflight policy to the turn's first request and the round-cap wrap-up request.
- [x] 6.3 Implement one forced-compaction recovery after a typed provider context overflow, rebuilding the request from the candidate and continuing the turn on success.
- [x] 6.4 Stop after a second overflow or an uncompactable plan with an actionable diagnostic; ensure ordinary transient retries cannot re-send an unchanged oversized request in a loop.
- [x] 6.5 Ensure mid-turn compaction still flushes before exact row boundaries are needed, but make post-compaction validity independent of whether the provider ever returned usage for the rejected request.

## 7. Persistence and Observability

- [x] 7.1 Add backward-compatible compaction metadata for measurement basis, pre/post estimates, target budget, retained-tail size, strategy, and tool summary/activity counts.
- [x] 7.2 Add the corresponding SQL migration with nullable/defaulted fields and tests proving old records remain readable.
- [x] 7.3 Persist updated observation fields on new records and log the trigger, pre/post budget, reduction strategy, and failure reason without logging full tool payloads or secrets.
- [x] 7.4 Keep `turn_usage.context_tokens` as the latest provider-observed round size while ensuring it is never treated as proof of next-request fit.

## 8. End-to-End Verification

- [x] 8.1 Add an integration test where the previous round is below the threshold, a large tool result is appended, preflight triggers, and the next request is compacted rather than sent over the hard limit.
- [x] 8.2 Add an integration test where the retained tail has large paired tool results; verify prose/reasoning survive, results become summaries, pairing stays valid, and the candidate fits.
- [x] 8.3 Add an integration test where the summary input itself would exceed the model window and chunked consolidation completes successfully.
- [x] 8.4 Add an integration test where a provider context-overflow error triggers one compaction and the rebuilt request succeeds; verify no unchanged retry occurs.
- [x] 8.5 Add an integration test where recovery is impossible; verify one bounded attempt, an actionable error, and no repeated ineffective compaction on the next request.
- [x] 8.6 Add a re-compaction test verifying the newest record holds merged tool activity and stitched sessions never replay superseded summaries.
- [x] 8.7 Add a model-switch test whose stored context exceeds the new model's budget; verify turn-start compaction uses the current model's budget.
- [x] 8.8 Run `go test ./internal/tokens/... ./internal/agent/... ./internal/service/... ./internal/handler/... ./test/integration/...`, `make test`, and `make lint`; record results in the change before archiving.

## 9. Documentation and Rollout

- [x] 9.1 Document the request-budget formula, safety margin, tail strategy, and tool-summary/ledger behavior in `openspec/specs` and relevant code comments.
- [x] 9.2 Document and validate the operator requirement that each `max_context_tokens` value matches the real gateway model window.
- [x] 9.3 Add a rollout check that compares configured windows with observed provider overflow diagnostics and flags clearly unsafe catalog entries.
- [x] 9.4 Update persisted-architecture and migration notes if new compaction fields or externalized tool-output retention are introduced.
