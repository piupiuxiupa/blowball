## ADDED Requirements

### Requirement: Create session explicitly
The system SHALL provide an authenticated endpoint for creating a new session. The server SHALL generate the session_id as a UUID v7 and persist the session row before returning it to the caller.

#### Scenario: Successful session creation
- **WHEN** an authenticated user sends POST /api/v1/sessions
- **THEN** the system returns HTTP 200 with body {"session_id": "<uuid7>"}

#### Scenario: Session row is persisted
- **WHEN** a session is created via POST /api/v1/sessions
- **THEN** a row exists in the sessions table with the generated session_id, the authenticated user_id, the current request trace_id, and default create_time/update_time

### Requirement: List session messages with pagination
The system SHALL allow an authenticated user to retrieve the full event stream of a session they own, paginated and ordered by (msg_time, msg_index).

#### Scenario: Retrieve first page
- **WHEN** an authenticated user sends GET /api/v1/sessions/:session_id/messages
- **THEN** the system returns HTTP 200 with a JSON object containing a "messages" array and a "next_page_token" field when more pages exist

#### Scenario: Cursor pagination
- **WHEN** a request includes a valid page_token query parameter
- **THEN** the system returns the next page of messages starting immediately after the cursor

#### Scenario: Page size limit
- **WHEN** a request includes page_size greater than the maximum allowed value
- **THEN** the system clamps page_size to the maximum (200) and returns results

#### Scenario: Unauthorized session access
- **WHEN** a user requests messages for a session that does not belong to them
- **THEN** the system returns HTTP 404

#### Scenario: Missing session
- **WHEN** a user requests messages for a non-existent session_id
- **THEN** the system returns HTTP 404

### Requirement: Message list response format
The system SHALL return each message in the response using the canonical message data model, including all event types, so the client can reconstruct the full interaction history.

#### Scenario: Full event stream returned
- **WHEN** a session contains user messages and assistant events
- **THEN** the response includes every stored message row with fields: id, session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, update_time

#### Scenario: Default ascending order
- **WHEN** no order parameter is provided
- **THEN** messages are ordered by (msg_time ASC, msg_index ASC)

#### Scenario: Descending order supported
- **WHEN** order=desc is provided
- **THEN** messages are ordered by (msg_time DESC, msg_index DESC)
