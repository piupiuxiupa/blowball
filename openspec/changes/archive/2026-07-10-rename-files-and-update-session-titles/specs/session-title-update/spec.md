# session-title-update Specification

## Purpose

定义会话标题的手动更新能力，包括鉴权、标题清理与持久化，以及通过 `titles.is_manual` 标记防止后续 AI 自动生成覆盖。

## ADDED Requirements

### Requirement: Update session title manually
The system SHALL provide an authenticated endpoint for a user to update the title of a session they own. The title SHALL be sanitized to a maximum of 20 characters and persisted to the `titles` table with `is_manual = TRUE`.

#### Scenario: Successful title update
- **WHEN** an authenticated user sends `PATCH /api/v1/sessions/:session_id` with body `{"title": "新标题"}` for a session they own
- **THEN** the system persists the title to the `titles` table with `is_manual = TRUE`, updates `sessions.update_time` to the current time, and returns HTTP 200 with body `{"session_id": "...", "title": "...", "update_time": "..."}`

#### Scenario: Title is truncated to 20 characters
- **WHEN** a user sends a title longer than 20 characters
- **THEN** the system stores only the first 20 characters and returns the truncated title in the response

#### Scenario: Session does not exist or belongs to another user
- **WHEN** a user sends `PATCH /api/v1/sessions/:session_id` for a missing session or one owned by another user
- **THEN** the system returns HTTP 404 with body `{"error": {"code": "NOT_FOUND", "message": "session not found"}}`

#### Scenario: Empty title
- **WHEN** a user sends an empty or whitespace-only title
- **THEN** the system returns HTTP 400 with body `{"error": {"code": "BAD_REQUEST", "message": "title is required"}}`

#### Scenario: Missing authentication
- **WHEN** a request is sent without a valid JWT
- **THEN** the system returns HTTP 401

## MODIFIED Requirements

(none)

## REMOVED Requirements

(none)
