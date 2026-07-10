# session-management Specification

## Purpose

定义会话管理能力，包括按需创建会话、SSE 流式响应、会话列表、自动标题生成、三层消息存储（Redis → 文件 → MySQL）、消息恢复降级策略以及消息数据模型。

## ADDED Requirements

(none)

## MODIFIED Requirements

### Requirement: Auto generate session title
The system SHALL asynchronously generate a short session title after the first user/assistant exchange, but SHALL NOT overwrite a title that was manually set by the user.

#### Scenario: Title generated after first exchange
- **WHEN** a user sends the first message in a new session and receives a complete assistant response
- **THEN** the system asynchronously calls OpenAI, generates a title of at most 20 characters, and writes it to the `titles` table with `is_manual = FALSE`

#### Scenario: Title generation failure
- **WHEN** the title generation call to OpenAI fails
- **THEN** the system uses the first 20 characters of the user message as the default title and logs a warning

#### Scenario: Manual title is not overwritten
- **WHEN** a user has previously set the session title via `PATCH /api/v1/sessions/:session_id`
- **THEN** the automatic title generation skips the LLM call and does not modify the `titles` row

## REMOVED Requirements

(none)
