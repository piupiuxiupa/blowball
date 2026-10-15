# memory-recall-replay Specification

## Purpose

Define the append-only replay semantics of turn-start memory recall (memory-recall-cache-stable): the synthetic `recall_memories` tool pair is persisted through the ordinary message/tool-call rows, replayed byte-identically by history recovery so turn-over-turn LLM payloads stay prefix-stable for the provider cache, deduplicated within a session by entry key, excluded from memory capture by sourcing, rendered through the ordinary tool-call transcript path, and treated as an ordinary tool pair by context compaction.

## Requirements

### Requirement: Recall results persist as a synthetic tool pair
When a memory-enabled turn's recall produces at least one new entry, the system SHALL inject the rendered frame as an `assistant` message carrying exactly one synthetic `recall_memories` tool call followed by a `tool` result message whose content is the rendered block, immediately before the turn's user message. Both sides SHALL be persisted through the existing message/tool-call persistence path in that order (pair before the user row), and SHALL NOT be registered as a callable tool in the model's current tool list.

#### Scenario: First turn persists and replays the pair
- **WHEN** the first memory-enabled turn of a session recalls entries
- **THEN** the sent context ends with `tool(recall_memories)` result, then the user message, and the persisted rows for the session contain the placeholder, tool-call, and user rows in that order

#### Scenario: The pair is not offered as a callable tool
- **WHEN** a turn with recall injection builds the model request
- **THEN** the request's tool list contains no `recall_memories` entry, while the historical pair remains in the message list

### Requirement: Turn-over-turn payloads are append-only between compactions
History recovery SHALL reconstruct every persisted `recall_memories` pair byte-identically to what was originally sent (same tool call id, arguments, and result content), and the next turn's payload SHALL equal the previous turn's full message list plus only: rows produced after it, the current turn's new pair (when there are new entries), and the new user message. No row already sent to the model SHALL be omitted, reordered, or re-rendered in a later turn except by compaction.

#### Scenario: Third turn reuses the first two turns verbatim
- **WHEN** a session with two completed memory-enabled turns sends a third message and no compaction has triggered
- **THEN** the third payload begins with a byte-identical copy of the first two payloads' message lists, and the only uncached suffix is the prior turn's new rows plus the current injection and user message

#### Scenario: No new entries means pure history extension
- **WHEN** recall returns only entries already injected earlier in the session
- **THEN** no tool pair is built, persisted, or sent, and the payload is exactly the recovered history plus the new user message

### Requirement: Session-scoped incremental dedupe
The system SHALL dedupe recall hits within a session: an entry whose key was already injected in a prior turn's `recall_memories` result SHALL be excluded from rendering. The key SHALL be the hit's memory URI when non-empty, otherwise a hash of the rendered entry text. The injected key set SHALL be derived from the `entries` list in the arguments of `recall_memories` calls present in the recovered history, requiring no separately stored session state. A session with no prior recall calls starts with an empty set.

#### Scenario: Already-injected URI is not repeated
- **WHEN** turn two's recall returns an entry whose URI appeared in turn one's pair
- **THEN** turn two's frame contains only entries not present in turn one's `entries` list

#### Scenario: Dedupe survives a server restart
- **WHEN** the process restarts between two turns of a session whose history is recovered from durable storage
- **THEN** the second turn's derived injected set still contains the first turn's entry keys

#### Scenario: Compaction may re-introduce summarized entries
- **WHEN** compaction summarizes a middle containing an earlier recall pair
- **THEN** the derived set no longer contains those keys and a still-relevant entry MAY be re-injected, bounded by the per-turn recall budget

### Requirement: Deterministic pair identity and ordering
The synthetic tool call id and placeholder row identity SHALL be deterministic functions of the session id and the turn's user-row client message id, so a retried send produces identical rows rather than duplicates. The pair SHALL be persisted before the turn's user row, and recall SHALL still complete before the send-time user-row write.

#### Scenario: Retried send does not duplicate the pair
- **WHEN** a client retries a send that already persisted the pair but not the assistant response
- **THEN** the retried turn reuses the same tool call id and does not append a second recall pair for the same user row

### Requirement: Capture and display semantics follow the tool shape
Memory capture SHALL remain sourced exclusively from the persisted user/assistant pair; `recall_memories` rows SHALL never be captured. The transcript UI SHALL render the pair through the existing tool-call card path, and compaction SHALL treat it like any other tool pair.

#### Scenario: Recall results are not re-captured
- **WHEN** a turn with a recall pair completes and capture runs
- **THEN** the OpenViking batch receives only the user content and top-level assistant text
