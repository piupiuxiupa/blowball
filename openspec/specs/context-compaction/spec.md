# context-compaction Specification

## Purpose

Define the context-compaction capability: when conversation context pressure reaches 80% of the request-selected catalog entry's `max_context_tokens` window, the compressible middle history is condensed into a structured checkpoint summary (first message and recent tail preserved) so long sessions never grow unbounded into the provider's hard window limit. Covers trigger conditions and authoritative measurement, range selection with tool-pairing boundaries, summary generation, compaction record storage (MySQL + Redis cache + session flag), stitched model context, the mid-turn pause-compact-continue flow, idempotent persistence, and best-effort failure degradation — with the display path untouched.
## Requirements
### Requirement: Compaction is disabled without a configured max context
The system SHALL treat a missing effective max-context value for the selected model as compaction-disabled: no trigger checks fire, no compaction records are written, and the model context path behaves exactly as before this capability existed. The effective max context for a turn SHALL be **exclusively** the request-selected catalog model's `max_context_tokens` (with the catalog mandatory, every turn resolves to an entry; the legacy top-level `openai.max_context_tokens` field no longer exists). A configured value MUST be a positive integer; any other value SHALL fail configuration load.

#### Scenario: Zero value disables compaction
- **WHEN** the selected model's effective max context is `0` (a catalog entry with `max_context_tokens: 0`)
- **THEN** no context pressure check runs and conversation turns behave identically to the pre-compaction system

#### Scenario: Invalid value fails config load
- **WHEN** a catalog entry's `max_context_tokens` is negative or non-integer
- **THEN** configuration loading returns an error and the server does not start

### Requirement: Context pressure is measured from authoritative token usage
The system SHALL use the most recent provider report's `prompt_tokens + completion_tokens` as the base for mid-turn pressure checks and SHALL add a conservative estimate for assistant/tool messages appended after that report. The system SHALL NOT use cumulative `turn_usage.total_tokens`.

The first request of each turn SHALL be evaluated from the complete rendered request, including the selected system prompt, tool descriptors, reconstructed or stitched messages, and current user message. `turn_usage.context_tokens` SHALL remain a provider-observed historical measurement and observability field; it SHALL NOT by itself determine whether the current turn's differently constructed request fits.

#### Scenario: Mid-turn measurement extends last round usage
- **WHEN** a tool round completes inside a turn and tool results are appended
- **THEN** the pressure check uses that round's provider usage plus the conservative appended-message estimate

#### Scenario: Turn-start measurement uses the built request
- **WHEN** a turn reaches its first model call after the agent has built its actual system prompt and tools
- **THEN** the preventive check measures that complete rendered request against the current model budget

#### Scenario: Cumulative turn usage is never the measure
- **WHEN** a turn made multiple LLM calls
- **THEN** the sum of all rounds' usage is not used as the context pressure measure

### Requirement: Compaction triggers at 80 percent of configured max context with a non-empty middle
The system SHALL trigger compaction when measured context reaches `0.8 × effective max context` — the request-selected model's `max_context_tokens` for that turn — AND the compressible middle (everything after the first message and before the retained tail) is non-empty. The 80 percent threshold is fixed and not configurable.

#### Scenario: Threshold reached with compressible middle
- **WHEN** measured context is at or above `0.8 × the selected model's max_context_tokens` and messages exist between the first message and the retained tail
- **THEN** compaction runs

#### Scenario: Threshold reached but middle is empty
- **WHEN** measured context reaches the threshold but the conversation consists only of the first message and the retained tail
- **THEN** compaction is skipped and the conversation continues unchanged

#### Scenario: Model switch changes the threshold
- **WHEN** the previous turn used a 400k-context model and ended at 300k context tokens, and the new request selects a 200k-context model
- **THEN** the turn-start check compares the stored 300k against `0.8 × 200k` and compaction runs before the first LLM call

### Requirement: Compaction preserves the first message, the last five agent messages, and tool pairing boundaries
The system SHALL select the retained tail by an estimated-token budget rather than a fixed message count. It SHALL prioritize recent user messages, assistant prose, and assistant reasoning, while preserving recent tool invocations as complete protocol units with their result bodies replaced by bounded deterministic summaries.

The system SHALL extract a checkpoint summary from the compressible middle before replacing that middle in the model-facing context. History persisted before compaction SHALL remain available through the unchanged message-history API.

The system SHALL preserve the original first-message anchor verbatim whenever that anchor fits the reserved anchor budget. When it does not fit, the system SHALL preserve a bounded head/tail excerpt of the first message together with its original size and a retrieval reference in the checkpoint; the persisted message row SHALL remain unchanged.

Every retained assistant message containing `tool_calls` SHALL be accompanied by one matching `role=tool` result per call. Parallel result sets and synthetic recall-memory pairs SHALL remain complete and together. The system SHALL NOT leave an orphan tool call or tool result in the model-facing context.

#### Scenario: Recent prose and reasoning survive with budgeted tail
- **WHEN** the most recent turns fit the retained-tail budget without any tool-result bodies
- **THEN** their user prose, assistant prose, and assistant reasoning are retained

#### Scenario: Boundary stays on a paired unit
- **WHEN** the planner chooses the final message to move into the checkpoint
- **THEN** the boundary row belongs to a complete tool invocation unit or a complete non-tool message, and the post-boundary tail reconstructs without orphans

#### Scenario: Large recent tool result becomes a summary
- **WHEN** a recent tool result would exceed its retained-result budget
- **THEN** its matching tool-call pair remains valid and the result body is replaced with a bounded deterministic summary and retrieval reference

#### Scenario: Tool pair is not split
- **WHEN** the tail boundary falls inside a tool invocation unit
- **THEN** the complete unit is either retained with summarized results or moved together into the shadowed span and checkpoint

#### Scenario: First message exceeds its anchor budget
- **WHEN** the original first message alone cannot fit in the anchor budget
- **THEN** the checkpoint retains a bounded complete message projection, original size, and retrieval reference while the persisted message row is untouched

#### Scenario: Anchor has multiple oversized fields
- **WHEN** an oversized anchor carries large content, reasoning, tool arguments, or inline images
- **THEN** the bounded projection removes image payloads and bounds every model-facing field rather than truncating only content

#### Scenario: Display history is untouched
- **WHEN** any compaction has occurred
- **THEN** the message-history API returns the complete persisted transcript with no summary rows or gaps

### Requirement: Summary generation uses the current agent model with a structured checkpoint template
The system SHALL generate the summary using the turn's request-selected model (the same model the three agents run for that turn) via the existing LLM client. The summarization instruction SHALL require a structured checkpoint (sections for primary intent, technical concepts, files and code, errors and fixes, pending jobs, current work, next step, critical context) preserving exact paths, commands, error strings, identifiers, and numeric values. Only returned text SHALL be retained; reasoning content and tool calls SHALL be discarded.

The system SHALL use sequential chunked consolidation when the complete shadowed transcript plus prior checkpoint does not fit the summary input budget. The fit decision SHALL use a conservative estimator that accounts for JSON-dense tool arguments and results in the mixed transcript. Each chunk SHALL reserve room for the fixed system prompt, prior or accumulated summary, and completion quota. A single summary call SHALL remain permissible when the complete input fits.

#### Scenario: Structured summary replaces the middle
- **WHEN** compaction completes
- **THEN** the shadowed middle is replaced in subsequent model contexts by a single user-role message framing the summary text

#### Scenario: Only text output is retained
- **WHEN** the summarization response contains reasoning content or tool calls alongside text
- **THEN** only the text is stored in the compaction record

#### Scenario: Summary call is observable
- **WHEN** the summarization LLM call executes through the OpenAI client
- **THEN** it is captured in `llm_raw_log` like any other call

#### Scenario: Small middle keeps one summary call
- **WHEN** the complete shadowed transcript plus prior checkpoint fits the summary input budget
- **THEN** the system may use a single summary call without chunking

#### Scenario: Large middle uses chunked consolidation
- **WHEN** the complete shadowed transcript plus prior checkpoint exceeds the summary input budget
- **THEN** the system processes budgeted chunks in order and retains one consolidated final checkpoint

#### Scenario: Exceptional payload is externalized
- **WHEN** one transcript payload prevents a summary chunk from fitting the summary input budget
- **THEN** the system summarizes or externalizes that payload and proceeds with the bounded chunk

### Requirement: Re-compaction merges the prior checkpoint instead of stacking
The system SHALL, when compacting a session that already has a compaction record, feed the prior summary together with the newly shadowed range into one summary request producing a single consolidated summary. Each compaction inserts a new record; the record with the highest `(session_id, id)` is authoritative for stitching, and historical records remain for audit.

#### Scenario: Second compaction consolidates
- **WHEN** compaction triggers on a session whose latest boundary precedes new shadowed content
- **THEN** the new summary is generated from the prior summary merged with the newly shadowed range, and the new record becomes the stitching source

#### Scenario: Append-only history
- **WHEN** a session is compacted twice
- **THEN** two rows exist in `context_compactions` and stitching reads only the newest

### Requirement: Compaction records cascade with session deletion
The `context_compactions` table SHALL enforce a foreign key to `sessions` with `ON DELETE CASCADE`. Deleting a session SHALL remove its compaction records without leaving orphans, matching the `turn_usage` precedent, and compaction records SHALL NOT be copied into the deletion archive mirror tables.

#### Scenario: Session deletion removes compaction records
- **WHEN** a session with compaction records is deleted
- **THEN** the rows are removed from `context_compactions` by cascade and no mirror rows are created

### Requirement: Session compaction flag gates stitching and is set last
The system SHALL persist the compaction mark on the session row (`context_compacted`) set to 1 after the compaction record is inserted and the Redis cache is written, in that order. The flag SHALL never be reset to 0. When the flag is 1 but no compaction record is recoverable from Redis or MySQL, the system SHALL fall back to full-history context and MUST NOT fail the turn.

#### Scenario: Write order is record, cache, then flag
- **WHEN** a compaction completes
- **THEN** the record insert and Redis write precede the session flag update

#### Scenario: Crash between writes leaves a harmless orphan
- **WHEN** the process dies after inserting the record but before setting the flag
- **THEN** subsequent turns treat the session as uncompacted and a later compaction simply inserts a newer authoritative row

#### Scenario: Missing record degrades to full history
- **WHEN** the flag is 1 but both the Redis key and the MySQL record are unavailable
- **THEN** the turn proceeds with full unreconstructed-compaction history instead of erroring

### Requirement: Redis caches the latest compaction record with backfill
The system SHALL cache the latest compaction record under `compaction:{session_id}` with the same TTL family as `msgs:{session_id}`. A new compaction SHALL overwrite the key. On cache miss the system SHALL read the latest MySQL row and backfill the cache. Session deletion SHALL clear the key.

#### Scenario: Cache hit serves stitching
- **WHEN** stitching runs and the Redis key exists
- **THEN** no MySQL read occurs for the compaction record

#### Scenario: Cache miss backfills from MySQL
- **WHEN** the Redis key is absent
- **THEN** the latest MySQL row is read and written back to the cache before stitching proceeds

### Requirement: Model context for compacted sessions is stitched from the compaction record
For a session marked compacted, the system SHALL construct the model context as: the first user message, then the framed summary as one user-role message, then every message after the recorded boundary cursor, then the current user message. The message-history API (`GET /sessions/:id/messages`), the `messages` table contents, and the `msgs:` Redis cache SHALL remain unchanged by compaction.

#### Scenario: Stitched context reaches the orchestrator
- **WHEN** a message is sent to a compacted session
- **THEN** the orchestrator receives first message + framed summary + post-boundary messages + the new user message, and nothing from the shadowed middle

#### Scenario: Display path is untouched
- **WHEN** any compaction has occurred
- **THEN** the message-history API returns exactly the rows persisted by normal turn flow, with no summary rows and no gaps

### Requirement: Mid-turn compaction flushes current turn data before compacting
When pressure triggers mid-turn, the system SHALL first persist the current turn's collected events (user message plus events so far) through the existing batch persistence path, then run compaction against the now-complete persisted history, then rewrite the in-memory conversation as first message + summary + retained tail and continue the agent loop. A flush failure SHALL abort that compaction attempt, log a warning, and continue the loop with the original context.

#### Scenario: Flush precedes summary
- **WHEN** mid-turn pressure triggers
- **THEN** the current turn's messages are persisted via the existing save path before the summarization call is made

#### Scenario: Loop continues on compacted context
- **WHEN** mid-turn compaction completes
- **THEN** subsequent rounds of the same turn run against the rewritten in-memory context

#### Scenario: Flush failure aborts compaction without killing the turn
- **WHEN** the mid-turn flush fails
- **THEN** compaction is skipped, a warning is logged, and the agent loop continues on the original context

### Requirement: Mid-turn flush and turn-end persistence are idempotent
Messages persisted by a mid-turn flush SHALL carry deterministic client message ids (`{trace_id}:{msg_index}`), and the turn-end batch save SHALL generate the same ids for the same messages so the MySQL unique index collapses duplicates. When a mid-turn flush occurred, the turn-end Redis dual-write SHALL push only the messages appended after the flush point.

#### Scenario: MySQL deduplicates re-saved messages
- **WHEN** a turn that flushed mid-turn completes and persists its full event stream
- **THEN** messages already flushed produce no duplicate `messages` rows

#### Scenario: Redis list receives only the suffix
- **WHEN** the turn-end save follows a mid-turn flush
- **THEN** `msgs:{session_id}` gains only the post-flush messages, not a second copy of the flushed prefix

### Requirement: Turn usage records end-of-turn context size
The system SHALL record, for every completed turn, the last LLM round's `prompt_tokens + completion_tokens` in `turn_usage.context_tokens` as a raw, model-independent size, enabling turn-start preventive compaction checks without estimation. The turn-start check SHALL compare this stored raw size against the CURRENT request's selected-model threshold.

#### Scenario: Context size persisted per turn
- **WHEN** a turn with at least one LLM call completes
- **THEN** its `turn_usage` row carries the final round's context size in `context_tokens`

#### Scenario: Turn start compacts before sending when context is already over threshold
- **WHEN** a new message is sent and the latest `turn_usage.context_tokens` is at or above `0.8 × the selected model's max_context_tokens` with a non-empty middle
- **THEN** compaction runs before the first LLM call of the turn

### Requirement: Compaction failures never block the conversation
Any compaction-stage failure (summary call error, empty summary, persistence error after the record insert) SHALL be handled as best-effort: log a warning, leave the conversation on the previous durable state, and let the turn proceed. Compaction SHALL NOT emit SSE events and SHALL NOT surface errors to the frontend.

#### Scenario: Summary failure skips compaction
- **WHEN** the summarization call fails or returns empty text
- **THEN** the turn continues with the original context and a warning is logged

#### Scenario: No frontend signal
- **WHEN** any compaction attempt runs, succeeds, or fails
- **THEN** no compaction-specific SSE event is emitted to the client

### Requirement: Stitched model context uses the validated compaction plan
For a compacted session, the system SHALL reconstruct the model context from the first-message anchor or bounded anchor, the framed checkpoint, the retained tail, and the current user message. The retained tail SHALL use the summarized result bodies and boundary recorded by the validated compaction plan. The system SHALL use the latest authoritative record and SHALL NOT replay superseded checkpoint summaries.

#### Scenario: Subsequent turn uses the validated stitched context
- **WHEN** a new message is sent to a compacted session
- **THEN** the orchestrator receives the validated anchor, framed checkpoint, summarized retained tail, and current user message

#### Scenario: Latest record wins
- **WHEN** a session has multiple compaction records
- **THEN** reconstruction uses the newest authoritative boundary and summary rather than an earlier superseded checkpoint

### Requirement: Retained tool results use deterministic summaries with stable identity
For each paired tool result retained in the model context, the system SHALL produce a bounded deterministic summary that includes the tool name, original tool call identity, success or error status, truncation state, a concise outcome, original approximate size, and a retrieval reference when full output is available. Dense JSON tool results SHALL use the conservative tool-content token estimate for the spill and truncation decisions. Error strings, exit codes, paths, commands, and identifiers that remain in the summary SHALL be preserved verbatim.

Known tool families SHALL use bounded, type-aware summaries. Unknown tools SHALL use a bounded head/tail excerpt plus status, size, and retrieval metadata. The system SHALL NOT fabricate details absent from the original result.

#### Scenario: Successful shell output is condensed
- **WHEN** a retained shell tool result contains a large command output
- **THEN** its result summary includes the exit code, key output facts, original size, and retrieval reference

#### Scenario: Failed tool output preserves the failure
- **WHEN** a retained tool result reports failure
- **THEN** its summary preserves the status and relevant exact error text instead of replacing them with a generic success description

#### Scenario: Unknown tool falls back deterministically
- **WHEN** a retained result comes from a tool family without a bespoke summarizer
- **THEN** the result is represented by a deterministic bounded head/tail excerpt with size and retrieval metadata

### Requirement: The checkpoint records earlier tool activity
When tool invocations leave the retained tail, the system SHALL consolidate them into a bounded tool-activity section of the checkpoint. The ledger SHALL include tool names, invocation counts, success and failure counts, important parameters, and concise outcomes. It SHALL preserve exact identifiers, paths, commands, and error strings needed for continuation.

The checkpoint SHALL remain a single consolidated summary. Repeated compaction SHALL merge prior checkpoint material and newer activity rather than stacking duplicate section inventories. The tool-activity ledger itself SHALL be condensed as it grows so the checkpoint cannot expand without bound.

#### Scenario: Calls are counted by tool
- **WHEN** multiple calls to the same tool leave the retained tail
- **THEN** the checkpoint reports their aggregate count and success/failure split

#### Scenario: Important arguments survive
- **WHEN** an earlier tool call used a consequential path, command, or query
- **THEN** that identifier or parameter appears verbatim in the tool-activity summary

#### Scenario: Re-compaction merges activity
- **WHEN** a compacted session is compacted again after further tool use
- **THEN** one consolidated checkpoint contains the merged tool activity without duplicate section inventories

### Requirement: Compaction candidates must fit the target request budget
Before saving an authoritative compaction record, the system SHALL validate the candidate model context against the target input budget. Validation SHALL require all tool pairs to be complete, the first-message anchor or bounded excerpt to be present, the framed checkpoint to appear exactly once, and only post-boundary messages to remain in the retained tail.

When a candidate does not fit, the system SHALL apply bounded deterministic reductions to the retained tail, tool-result summaries, and tool-activity ledger. If the irreducible context still does not fit, the system SHALL report an uncompactable-context outcome with a diagnostic reason and SHALL NOT publish an ineffective authoritative compaction record.

#### Scenario: Candidate passes fit validation
- **WHEN** the first-message anchor, checkpoint, and retained tail fit the input budget
- **THEN** the record is eligible to become authoritative and subsequent requests use the candidate context

#### Scenario: Candidate is reduced before publication
- **WHEN** the first candidate exceeds the target input budget but further tail reduction can make it fit
- **THEN** the planner reduces retained material and validates the reduced candidate before recording success

#### Scenario: A requested compaction is skipped for an empty middle
- **WHEN** request budgeting reaches its trigger but the conversation has no compressible middle
- **THEN** the system records a structured no-op reason and does not claim that compaction succeeded

#### Scenario: Irreducible context fails explicitly
- **WHEN** the minimal valid anchor and checkpoint still exceed the provider input budget
- **THEN** no oversized compaction record becomes authoritative and the turn reports an actionable uncompactable-context failure

### Requirement: Compaction records expose budget outcomes
Each new compaction record SHALL persist its measurement basis, pre-compaction request estimate, target input budget, post-compaction request estimate, retained tail size, summary strategy, and tool-summary or activity counts. New observation fields SHALL have backward-compatible defaults so existing records remain readable.

#### Scenario: Successful compaction is auditable
- **WHEN** a compaction candidate becomes authoritative
- **THEN** its record shows both the pressure before compaction and the estimated fit after compaction

#### Scenario: Legacy record remains readable
- **WHEN** a session loads a compaction record created before these fields existed
- **THEN** the record is treated as valid legacy data without a migration failure

