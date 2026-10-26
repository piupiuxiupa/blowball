## Why

Current compaction can report success without making the next model request fit: it preserves a fixed five-message tail byte-for-byte, performs no post-compaction budget check, and measures pressure before the next tool results are added. Large tool outputs can therefore cross the provider's hard window before compaction runs or survive inside the retained tail, causing later turns to continue failing with `input tokens exceed the model context limit`.

We need compaction to produce a demonstrably bounded request while retaining the information a continuing assistant actually uses: recent user/assistant prose and reasoning, exact tool-call identities and outcomes, and access paths to full tool output.

## What Changes

- Convert the retained tail from a fixed message count into a token-budgeted window that prioritizes recent user content, assistant prose, and assistant reasoning.
- Keep assistant `tool_calls` and `role=tool` results paired. Replace large retained tool-result bodies with deterministic structured summaries containing status, key outcome, size/truncation state, and a workspace or persisted-output reference.
- Add a compact tool-activity ledger to the checkpoint for calls moved out of the retained tail: tool names, invocation counts, success/failure counts, important parameters, and short outcome summaries.
- Add per-tool summarizers for common result families, with a deterministic head/tail-plus-size fallback for unknown tools.
- Validate a compaction candidate before publishing it: estimated request size must fit the target input budget, tool pairing must remain valid, and the first-message anchor plus checkpoint must remain present.
- If the candidate still does not fit, progressively reduce the retained tail, tighten summaries, and externalize large results rather than saving an ineffective compaction record.
- Estimate every next LLM request before sending it, including new assistant tool-call and tool-result messages appended after the previous round's usage.
- Reserve room for the configured completion quota and a safety margin in the input budget.
- Recognize provider context-overflow errors distinctly from generic 400 responses and attempt one bounded force-compaction recovery of the same request; do not retry the unchanged oversized request.
- Record pre-compaction, target-budget, and post-compaction context sizes for diagnosis.

## Capabilities

### New Capabilities

- `context-request-budgeting`: per-request context measurement, completion-quota reservation, provider context-overflow classification, and bounded forced-recovery before a turn terminates.

### Modified Capabilities

- `context-compaction`: budgeted retained-tail selection, tool-result summarization and activity accounting, tool-pair preservation, post-compaction fit validation, and outcome observability.

## Impact

- Agent request loop and round hooks: preflight budgeting, overflow recovery, and updated compaction inputs/outputs.
- Compaction service and persistence: budgeted planning, tool-result summarization, candidate validation, and additional size/strategy metadata.
- Tool and workspace adapters: deterministic result-summary formats and external-output references for large or unknown results.
- Token estimation utilities: conservative estimates for mixed prose, JSON, and tool envelopes, with tests alongside gateway-observed usage.
- No client-visible API contract change is expected. Existing message-history display behavior remains unchanged: compaction affects model context only, not persisted transcript rows.
- Existing compaction records remain readable. Any new record fields must have backward-compatible defaults or explicit nil handling.

## Out of Scope

- Replacing the model's native tokenizer with a bundled tokenizer for every gateway model.
- Semantic summarization of every retained tool result on every LLM request.
- Guaranteeing progress when one irreducibly required payload exceeds the provider window even after externalization and summary; that case will fail with an actionable uncompactable-context error instead of looping.
