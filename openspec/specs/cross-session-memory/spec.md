# cross-session-memory Specification

## Purpose

Define the cross-session long-term memory capability: per-user memory backed by an external OpenViking server, operating fully automatically around the chat turn (turn-start recall injection into the LLM context + turn-end capture submission for server-side memory extraction). Covers the trusted-gateway per-user isolation model, recall rendering and budget, capture content and sequence, session-id mapping, failure degradation, and the default-off configuration contract. The agent layer, the display path, and the api role are untouched.

## Requirements

### Requirement: Memory is disabled by default
The system SHALL treat an omitted `memory:` block, or `memory.enabled: false`, as the capability being off: no recall query is issued, no capture is submitted, no memory client is constructed, and every turn behaves byte-for-byte as before this capability. When enabled, `memory.base_url` MUST be a non-empty absolute http(s) URL; negative `recall_limit` / `recall_token_budget` / `max_capture_bytes` / `recall_timeout` / `capture_timeout` values SHALL fail configuration load. A DISABLED block carrying stale or invalid values SHALL still load (validation guards only the enabled state).

#### Scenario: Omitted block is zero behavior change
- **WHEN** the configuration has no `memory:` block
- **THEN** no OpenViking request is ever made and the streaming message path is identical to the pre-capability system

#### Scenario: Enabled without base_url fails load
- **WHEN** `memory.enabled: true` is set without a `memory.base_url`
- **THEN** configuration loading returns an error and the server does not start

#### Scenario: Disabled block with stale values loads
- **WHEN** `memory.enabled: false` carries an invalid `base_url` or negative knobs
- **THEN** configuration loading succeeds

### Requirement: Per-user memory isolation via the trusted-gateway identity headers
The system SHALL scope every OpenViking request to the requesting blowball user by sending the user's identity in the `X-OpenViking-User` header (account fixed by `memory.account`, one operator `memory.api_key` for the whole deployment; the key MAY be empty against a local no-auth server). The user identity SHALL be derived from the authenticated user_id: ids matching `^[A-Za-z0-9._-]+$` (UUID v7) pass through verbatim; any other id SHALL map deterministically to a SHA-256 digest (never character replacement, which could alias two users onto one tenant). One user's requests SHALL never be servable from another user's memory namespace.

#### Scenario: Two users never share a namespace
- **WHEN** two different authenticated users each send a message
- **THEN** their recall queries and captures carry different `X-OpenViking-User` values and the server isolates their stored memories accordingly

#### Scenario: Non-conforming user id is hash-mapped injectively
- **WHEN** a user_id contains characters outside the OpenViking-safe set
- **THEN** it maps to a stable hash-derived tenant id and two distinct such ids never map to the same value

### Requirement: Turn-start recall injects an ephemeral block
On every streaming message send with memory enabled, the system SHALL query the user's memory namespace (semantic search scoped to `viking://~/memories`, which the server expands to the authenticated user's space, context type memory, `memory.recall_limit` entries, deadline `memory.recall_timeout`) using the new message content as the query, and when hits survive rendering SHALL inject the rendered block as a user-role message immediately before the new user message in the turn's LLM context. The block SHALL be ephemeral: never persisted to the messages store, never present in history recovery output, and never re-captured (capture content is sourced exclusively from the persisted pair). Rendering SHALL sort entries by score descending, prefer the entry Abstract over the Overview, cap each entry's rendered length, and bound the aggregate block by `memory.recall_token_budget` using the shared CJK-aware estimate, always including the top-scored entry. Recall SHALL complete before the send-time user-row write (the history-read invariant).

#### Scenario: Relevant memory is injected before the user message
- **WHEN** the user sends a message and the memory store returns hits for it
- **THEN** the orchestrator receives a `<user-memories>` framed user-role message immediately before the new user message

#### Scenario: Nothing relevant means no injection
- **WHEN** the query returns no memories or none survive rendering
- **THEN** no extra message is injected and the context is exactly the pre-capability shape

#### Scenario: The recall block never reaches persistence
- **WHEN** a turn with recall injection completes and its messages are persisted
- **THEN** no persisted row (Redis write-behind batch or MySQL) contains the recall frame

### Requirement: Turn-end capture on both terminal paths
On every turn terminal state (success, error, or cancellation), the system SHALL asynchronously submit the turn's exchange to OpenViking as a fire-and-forget operation on a detached context: get-or-create the session (`bb-{session_id}`), batch-append the user message (always) plus the top-level assistant text (skipped when the turn produced none), and commit with `keep_recent_count: 0` (archiving pending messages and triggering server-side asynchronous memory extraction; the extraction task is NOT polled). The assistant content SHALL be the merged top-level token runs only (sub-agent output, reasoning, and tool events excluded). Each message body SHALL be truncated with an explicit marker at `memory.max_capture_bytes`. Failures SHALL be logged as warnings and SHALL NOT affect the turn, its persistence, or the response. A capture racing graceful shutdown is at-least-once (landed messages stay pending on the session and the session's next turn commits them).

#### Scenario: Successful turn is captured
- **WHEN** a turn completes successfully
- **THEN** the OpenViking session for `bb-{session_id}` receives the user content and the top-level assistant text, then a commit

#### Scenario: Interrupted turn still captures the exchange
- **WHEN** a turn errors or is cancelled after assistant tokens streamed
- **THEN** the capture still submits the user message and whatever top-level assistant text exists

#### Scenario: Sub-agent output is not captured as the assistant
- **WHEN** a turn dispatched sub-agents whose token events carry a run id
- **THEN** the captured assistant text excludes those runs

#### Scenario: Capture failure does not disturb the turn
- **WHEN** the OpenViking server is unreachable at turn end
- **THEN** the turn's persistence completes unchanged and a warning is logged

### Requirement: Same-session captures are serialized by the run claim
The system SHALL rely on the existing Redis session-run claim to serialize a blowball session's turns, so batch/commit pairs against the same OpenViking session id never race. Concurrent turns of DIFFERENT sessions by the same user SHALL be safe (distinct session ids; the shared HTTP transport is concurrency-safe).

#### Scenario: Sequential turns of one session
- **WHEN** a user sends consecutive messages to one session
- **THEN** each capture lands on the same `bb-{session_id}` in turn order

### Requirement: Degradation never gates the chat
The system SHALL treat every memory operation as best-effort: recall failure or timeout skips injection with a WARN; capture failure logs a WARN; the startup health probe (bounded, operator-scoped) logs a WARN on failure and SHALL NOT prevent boot (contrast the MCP manager's fail-fast). Memory is wired only in the agent/all roles; the api role never constructs the service.

#### Scenario: Server down at startup
- **WHEN** `memory.enabled: true` and the OpenViking server is unreachable at boot
- **THEN** the server starts, a WARN is logged, and turns proceed without memory until the server returns

#### Scenario: Slow recall is bounded
- **WHEN** the recall query exceeds `memory.recall_timeout`
- **THEN** the query is abandoned, no block is injected, and the turn proceeds
