## MODIFIED Requirements

### Requirement: Turn-over-turn payloads are append-only between compactions
History recovery SHALL reconstruct every persisted `recall_memories` pair byte-identically to what was originally sent (same tool call id, arguments, and result content), and the next turn's durable canonical payload SHALL equal the previous turn's canonical message list plus only: rows produced after it, the current turn's new pair (when there are new entries), and the new user message. No canonical row already sent to the model SHALL be omitted, reordered, or re-rendered in a later turn except by compaction.

Each synthetic recall pair SHALL remain an independent assistant message containing exactly its synthetic call followed by its matching tool result. Recovery SHALL NOT merge it into a previous turn's assistant tool_calls array or attach the previous turn's content/reasoning to its assistant message, including when the previous turn ends at completed tool results without subsequent assistant text.

Cache-only runtime status SHALL follow `agent-runtime-status` independently of durable recall rows. When the status log is valid, replay SHALL preserve the previous full live prefix and original legal anchors; a previous turn's budget SHALL remain after that turn's complete tool unit and before the next turn's synthetic recall unit. The current status SHALL precede the real current user message without splitting the recall assistant/tool pair. A status cache miss or invalid log SHALL begin a new cache epoch without modifying or partially replaying the durable recall pair.

#### Scenario: Third turn reuses the first two turns verbatim
- **WHEN** a session with two completed memory-enabled turns sends a third message and no compaction has triggered
- **THEN** the third canonical payload begins with a byte-identical copy of the first two canonical payloads' message lists, and the only uncached suffix is the prior turn's new rows plus the current injection and user message

#### Scenario: No new entries means pure history extension
- **WHEN** recall returns only entries already injected earlier in the session
- **THEN** no tool pair is built, persisted, or sent, and the canonical payload is exactly the recovered history plus the new user message

#### Scenario: A completed tool tail cannot absorb the next recall pair
- **WHEN** a previous turn ends at completed call A and the next turn persists synthetic recall-B before its user row
- **THEN** recovery emits `assistant[A] → tool[A] → assistant[recall-B] → tool[recall-B] → user`
- **AND THEN** call A and recall-B keep their original IDs, arguments, results, and independent assistant messages

#### Scenario: Runtime budget replay does not split a new recall pair
- **WHEN** recovery contains a previous completed tool unit, the next turn's recall pair, and a valid status log anchored after the previous unit's final result
- **THEN** the active transcript is `assistant[A] → tool[A] → budget[A] → assistant[recall-B] → tool[recall-B] → current_status → user`
- **AND THEN** no synthetic user status occurs between either assistant tool_calls message and its complete matching results

#### Scenario: Invalid runtime status does not change durable recall
- **WHEN** the cache-only status log is missing or rejected for an invalid anchor
- **THEN** the recall pair is still recovered byte-identically and independently from the previous tool unit
- **AND THEN** a fresh current status is added before the real user under the new cache epoch, without moving or rewriting canonical recall rows
