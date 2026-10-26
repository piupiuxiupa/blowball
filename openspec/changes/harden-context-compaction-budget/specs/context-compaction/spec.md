## MODIFIED Requirements

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
- **THEN** the checkpoint retains a bounded excerpt, original size, and retrieval reference while the persisted message row is untouched

#### Scenario: Display history is untouched
- **WHEN** any compaction has occurred
- **THEN** the message-history API returns the complete persisted transcript with no summary rows or gaps

### Requirement: Summary generation uses the current agent model with a structured checkpoint template
The system SHALL generate the summary using the turn's request-selected model (the same model the three agents run for that turn) via the existing LLM client. The summarization instruction SHALL require a structured checkpoint (sections for primary intent, technical concepts, files and code, errors and fixes, pending jobs, current work, next step, critical context) preserving exact paths, commands, error strings, identifiers, and numeric values. Only returned text SHALL be retained; reasoning content and tool calls SHALL be discarded.

The system SHALL use sequential chunked consolidation when the complete shadowed transcript plus prior checkpoint does not fit the summary input budget. Each chunk SHALL reserve room for the fixed system prompt, prior or accumulated summary, and completion quota. A single summary call SHALL remain permissible when the complete input fits.

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

## ADDED Requirements

### Requirement: Stitched model context uses the validated compaction plan
For a compacted session, the system SHALL reconstruct the model context from the first-message anchor or bounded anchor, the framed checkpoint, the retained tail, and the current user message. The retained tail SHALL use the summarized result bodies and boundary recorded by the validated compaction plan. The system SHALL use the latest authoritative record and SHALL NOT replay superseded checkpoint summaries.

#### Scenario: Subsequent turn uses the validated stitched context
- **WHEN** a new message is sent to a compacted session
- **THEN** the orchestrator receives the validated anchor, framed checkpoint, summarized retained tail, and current user message

#### Scenario: Latest record wins
- **WHEN** a session has multiple compaction records
- **THEN** reconstruction uses the newest authoritative boundary and summary rather than an earlier superseded checkpoint

### Requirement: Retained tool results use deterministic summaries with stable identity
For each paired tool result retained in the model context, the system SHALL produce a bounded deterministic summary that includes the tool name, original tool call identity, success or error status, truncation state, a concise outcome, original approximate size, and a retrieval reference when full output is available. Error strings, exit codes, paths, commands, and identifiers that remain in the summary SHALL be preserved verbatim.

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

