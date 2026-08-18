# context-compaction Delta Specification

## ADDED Requirements

### Requirement: Compaction is disabled without a configured max context
The system SHALL treat a zero or missing `openai.max_context_tokens` value as compaction-disabled: no trigger checks fire, no compaction records are written, and the model context path behaves exactly as before this capability existed. A configured value MUST be a positive integer; any other value SHALL fail configuration load.

#### Scenario: Zero value disables compaction
- **WHEN** `openai.max_context_tokens` is unset or `0`
- **THEN** no context pressure check runs and conversation turns behave identically to the pre-compaction system

#### Scenario: Invalid value fails config load
- **WHEN** `openai.max_context_tokens` is negative or non-integer
- **THEN** configuration loading returns an error and the server does not start

### Requirement: Context pressure is measured from authoritative token usage
The system SHALL measure context pressure using authoritative provider usage, never character estimation and never the cumulative `turn_usage.total_tokens`. Mid-turn checks SHALL use the most recent LLM response's `prompt_tokens + completion_tokens`; turn-start preventive checks SHALL use the latest `turn_usage.context_tokens` value.

#### Scenario: Mid-turn measurement uses last round usage
- **WHEN** a tool round completes inside a turn
- **THEN** the pressure check compares the just-finished round's `prompt_tokens + completion_tokens` against `0.8 × max_context_tokens`

#### Scenario: Turn-start measurement uses recorded context size
- **WHEN** a new message is sent to a session with prior turns
- **THEN** the preventive check reads the latest `turn_usage.context_tokens` row instead of estimating from content length

#### Scenario: Cumulative turn usage is never the measure
- **WHEN** a turn made multiple LLM calls
- **THEN** the sum of all rounds' usage is not used as the context pressure measure

### Requirement: Compaction triggers at 80 percent of configured max context with a non-empty middle
The system SHALL trigger compaction when measured context reaches `0.8 × max_context_tokens` AND the compressible middle (everything after the first message and before the retained tail) is non-empty. The 80 percent threshold is fixed and not configurable.

#### Scenario: Threshold reached with compressible middle
- **WHEN** measured context is at or above `0.8 × max_context_tokens` and messages exist between the first message and the retained tail
- **THEN** compaction runs

#### Scenario: Threshold reached but middle is empty
- **WHEN** measured context reaches the threshold but the conversation consists only of the first message and the retained tail
- **THEN** compaction is skipped and the conversation continues unchanged

### Requirement: Compaction preserves the first message, the last five agent messages, and tool pairing boundaries
The system SHALL select the compaction range on the reconstructed agent-message sequence: the first user message is never compacted, the last five agent messages (complete conversation units after `MessagesToAgentMessages`) are retained verbatim, and everything between them is shadowed. The range boundary SHALL NOT split an assistant message carrying tool calls from its corresponding tool result messages. The boundary SHALL be persisted as the composite cursor `(msg_time, msg_index, id)` of the last shadowed persistence row.

#### Scenario: First message always survives
- **WHEN** any compaction completes
- **THEN** the reconstructed model context still begins with the session's first user message verbatim

#### Scenario: Last five agent messages retained verbatim
- **WHEN** compaction completes
- **THEN** the five agent messages preceding the cut point appear in subsequent model contexts exactly as reconstructed

#### Scenario: Boundary never splits a tool pair
- **WHEN** the nominal fifth-from-last agent message is a tool result whose paired assistant tool-call message would fall inside the shadowed range
- **THEN** the retained tail is extended backward so the call/result pair stays together on one side of the boundary

#### Scenario: Boundary stored as composite cursor
- **WHEN** a compaction record is written
- **THEN** it carries the shadowed range's final persistence row as `(boundary_msg_time, boundary_msg_index, boundary_msg_id)`

### Requirement: Summary generation uses the current agent model with a structured checkpoint template
The system SHALL generate the summary using the orchestrating agent's (Confucius) configured model via the existing LLM client. The summarization instruction SHALL require a structured checkpoint (sections for primary intent, technical concepts, files and code, errors and fixes, pending jobs, current work, next step, critical context) preserving exact paths, commands, error strings, identifiers, and numeric values. Only returned text SHALL be retained; reasoning content and tool calls SHALL be discarded.

#### Scenario: Structured summary replaces the middle
- **WHEN** compaction completes
- **THEN** the shadowed middle is replaced in subsequent model contexts by a single user-role message framing the summary text

#### Scenario: Only text output is retained
- **WHEN** the summarization response contains reasoning content or tool calls alongside text
- **THEN** only the text is stored in the compaction record

#### Scenario: Summary call is observable
- **WHEN** the summarization LLM call executes through the OpenAI client
- **THEN** it is captured in `llm_raw_log` like any other call

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
The system SHALL record, for every completed turn, the last LLM round's `prompt_tokens + completion_tokens` in `turn_usage.context_tokens`, enabling turn-start preventive compaction checks without estimation.

#### Scenario: Context size persisted per turn
- **WHEN** a turn with at least one LLM call completes
- **THEN** its `turn_usage` row carries the final round's context size in `context_tokens`

#### Scenario: Turn start compacts before sending when context is already over threshold
- **WHEN** a new message is sent and the latest `turn_usage.context_tokens` is at or above `0.8 × max_context_tokens` with a non-empty middle
- **THEN** compaction runs before the first LLM call of the turn

### Requirement: Compaction failures never block the conversation
Any compaction-stage failure (summary call error, empty summary, persistence error after the record insert) SHALL be handled as best-effort: log a warning, leave the conversation on the previous durable state, and let the turn proceed. Compaction SHALL NOT emit SSE events and SHALL NOT surface errors to the frontend.

#### Scenario: Summary failure skips compaction
- **WHEN** the summarization call fails or returns empty text
- **THEN** the turn continues with the original context and a warning is logged

#### Scenario: No frontend signal
- **WHEN** any compaction attempt runs, succeeds, or fails
- **THEN** no compaction-specific SSE event is emitted to the client
