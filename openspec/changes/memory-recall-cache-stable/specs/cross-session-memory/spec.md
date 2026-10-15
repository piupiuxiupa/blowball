## ADDED Requirements

### Requirement: Turn-start recall injects an incremental persisted tool pair
On every streaming message send with memory enabled, the system SHALL query the user's memory namespace (semantic search scoped to `viking://~/memories`, context type memory, `memory.recall_limit` entries, deadline `memory.recall_timeout`) using the new message content as the query. Hits whose dedupe key was already injected earlier in the session SHALL be excluded before rendering; when new hits survive rendering the system SHALL inject the rendered block as a synthetic `recall_memories` tool-call/tool-result pair immediately before the new user message and persist both sides (see the memory-recall-replay capability). Rendering SHALL sort entries by score descending, prefer the entry Abstract over the Overview, cap each entry's rendered length, and bound the aggregate block by `memory.recall_token_budget` using the shared CJK-aware estimate, always including the top-scored new entry. Recall SHALL complete before the send-time user-row write (the history-read invariant).

#### Scenario: Relevant new memory is injected before the user message
- **WHEN** the user sends a message and the memory store returns hits not yet injected this session
- **THEN** the orchestrator receives a `recall_memories` tool result containing the `<user-memories>` framed block immediately before the new user message

#### Scenario: Nothing new means no injection
- **WHEN** the query returns no memories, none survive rendering, or every survivor was already injected this session
- **THEN** no tool pair is built and the context is exactly the pre-capability shape plus previously persisted recall pairs replayed from history

## REMOVED Requirements

### Requirement: Turn-start recall injects an ephemeral block
**Reason**: The ephemeral end-slot block makes turn payloads non-append-only: the slot the block occupied in turn N carries the persisted user message in turn N+1, so the prefix cache diverges there every turn — each recall block is billed once at full input price and never reused, and the previous exchange is re-billed every turn.
**Migration**: Recall results are now persisted as a synthetic `recall_memories` tool pair, replayed byte-identically by history recovery, and deduplicated within the session (see the memory-recall-replay capability and the ADDED requirement above). The rendered frame, query semantics, budget, and failure degradation are unchanged.
