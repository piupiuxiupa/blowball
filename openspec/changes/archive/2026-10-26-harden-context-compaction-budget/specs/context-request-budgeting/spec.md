## ADDED Requirements

### Requirement: Every model request has a calculated input budget
The system SHALL calculate an input budget for each model request as the selected model's context window minus its configured completion quota and a documented safety margin. A non-positive remaining budget SHALL fail configuration or request admission before an LLM call is made.

The system SHALL verify that each catalog entry's configured window is the operator-declared real gateway window; the request budget SHALL use that value consistently for compaction and provider-overflow decisions.

#### Scenario: Completion quota is reserved
- **WHEN** a model has a 200,000-token window and an 8,192-token completion quota
- **THEN** the input budget excludes both the completion quota and the configured safety margin

#### Scenario: Invalid remaining budget is rejected
- **WHEN** completion quota plus safety margin leave no positive input budget
- **THEN** the request is rejected before any model call

### Requirement: The next request is estimated after each tool round
Before each LLM request, the system SHALL estimate the complete request currently being sent, including system prompt, tools, conversation messages, and protocol overhead. Mid-turn budget extensions SHALL use the latest provider-observed prompt and completion usage plus conservatively estimated newly appended assistant and tool messages. Provider usage SHALL be treated as data and SHALL NOT be overloaded as a forced-compaction control signal.

The estimator SHALL apply a conservative bias for JSON-dense and tool-envelope content. The system SHALL NOT rely solely on the previous round's provider usage when tool results have been appended since that measurement.

#### Scenario: Tool result changes the estimate
- **WHEN** a round returns provider usage under the threshold and the agent then appends a large tool result
- **THEN** the pre-request estimate includes that result before compaction is evaluated

#### Scenario: First request has no prior usage
- **WHEN** a new turn has no provider observation to extend
- **THEN** the system estimates the complete rendered request and applies the input budget before the first provider call

#### Scenario: Provider reports zero usage
- **WHEN** a provider response omits usage or reports zero prompt and completion tokens
- **THEN** the system records that observation without treating it as a forced-compaction command

#### Scenario: Preflight compaction preserves live-only current-turn data
- **WHEN** preflight compaction rewrites a context containing live inline message fields that persistence reconstructs only as placeholders
- **THEN** the rewritten request retains those live fields while bounding any replayed text

#### Scenario: Live overlay is part of candidate fit
- **WHEN** a compaction candidate will later be overlaid with live inline data
- **THEN** tail selection materializes the same replacement/summarized-replay projection the next request will use and measures its actual tokens before publication

#### Scenario: Live message matching is unambiguous
- **WHEN** a live image message corresponds to a reconstructed placeholder
- **THEN** matching uses its durable source position and persisted placeholder identity, a retained occurrence is replaced in place, and a summarized occurrence never overwrites an older same-content message

#### Scenario: Live input has no durable counterpart
- **WHEN** a live message is absent from durable reconstruction and therefore from the checkpoint
- **THEN** the overlay keeps its full content and the candidate is rejected if that content does not fit, rather than truncating and claiming it was preserved elsewhere

#### Scenario: Current question survives mid-turn and repeated compaction
- **WHEN** tool messages follow the current user message or an earlier compaction has already rewritten it as a bounded replay
- **THEN** every compaction attempt reuses the full live input captured at turn start, without recollecting from or locating it in the mutable round

#### Scenario: Final restore does not reclassify against the live round
- **WHEN** the compaction hook returns the completed durable projection with live data already materialized
- **THEN** that hook result carries an explicit overlay-applied marker and agent-side restore leaves it unchanged, including full unmatched messages and bounded checkpoint replays

#### Scenario: Repeated same-text prompts remain distinct occurrences
- **WHEN** the current live message has the same text as an older user message
- **THEN** the applied marker decides whether restore runs, and content equality alone never proves that the current occurrence is present

### Requirement: Compaction is triggered before a request exceeds the budget
The system SHALL begin compaction when either the existing percentage threshold or the preflight request-budget threshold is reached, and the compressible middle is non-empty. The effective trigger SHALL be the lower of the applicable safe thresholds. The system SHALL reserve space for the completion quota and the next appended message set.

#### Scenario: Preflight threshold wins
- **WHEN** the next-request estimate would approach the input budget sooner than 80% of the full window
- **THEN** compaction is attempted before that request is sent

#### Scenario: Empty middle remains a no-op
- **WHEN** the budget threshold is reached but no compressible middle exists
- **THEN** the system records a structured no-op reason and does not claim that compaction succeeded

### Requirement: Provider context overflow triggers one bounded forced recovery
The system SHALL distinguish provider context-overflow errors from generic request errors and transient failures. On a context-overflow response, it SHALL attempt one forced compaction against the current turn snapshot and rebuild the request from the compacted context.

The system SHALL NOT retry the unchanged oversized request through the ordinary transient retry path. If the rebuilt request overflows again or the context is uncompactable, the turn SHALL fail with an actionable context-overflow or uncompactable-context error and SHALL persist enough diagnostic state to avoid repeating the same ineffective attempt indefinitely.

#### Scenario: Overflow recovers after forced compaction
- **WHEN** the provider rejects a request for exceeding its context window and a valid compacted candidate can be built
- **THEN** the system sends the rebuilt compacted request and continues the turn

#### Scenario: Generic 400 is not treated as overflow
- **WHEN** the provider returns a 400 for a malformed or unsupported request rather than context overflow
- **THEN** the system preserves the semantic error and does not force compaction

#### Scenario: Second overflow stops with a diagnostic
- **WHEN** the rebuilt request is also rejected for context overflow
- **THEN** the system stops bounded recovery and reports an actionable failure containing both the latest estimate and effective input budget

#### Scenario: Wrap-up overflow uses the same bounded recovery
- **WHEN** the tool-disabled round-cap wrap-up request is rejected for context overflow
- **THEN** the system attempts one forced compaction and rebuilds the wrap-up request before reporting exhaustion

#### Scenario: Rebuilt wrap-up also overflows
- **WHEN** the rebuilt wrap-up request is rejected for context overflow
- **THEN** the diagnostic reports the latest rebuilt-request estimate together with the effective input budget

### Requirement: Request-budget outcomes are observable
The system SHALL record or emit structured diagnostics for the selected model window, completion reservation, safety margin, estimated request size, measurement basis, compaction trigger, pre/post compaction size, and the reason for any uncompactable result. Diagnostics SHALL NOT expose full tool-result payloads or secrets.

#### Scenario: Overflow failure explains its budget
- **WHEN** a request cannot be made to fit
- **THEN** the diagnostic identifies the effective input budget, latest estimate, and whether compaction was attempted
