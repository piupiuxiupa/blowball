-- 005_messages_event_type.sql
-- Extend messages table to store the raw assistant event stream.
-- event_type distinguishes user messages from streaming events (tokens,
-- tool_calls, agent lifecycle markers). role becomes nullable because marker
-- events (agent_start/agent_end) carry no OpenAI role semantics. msg_time is
-- promoted to millisecond precision so events within the same second can be
-- ordered reliably via msg_index.

ALTER TABLE `messages`
  ADD COLUMN `event_type` VARCHAR(16) NOT NULL DEFAULT 'token'
    COMMENT 'message | token | tool_call | agent_start | agent_end | agent_error'
    AFTER `role`,
  MODIFY `role` VARCHAR(16) NULL
    COMMENT 'OpenAI role (user/assistant/tool); NULL for marker events',
  MODIFY `msg_time` TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
    COMMENT 'When the message/event was produced (millisecond precision)';

-- Backfill existing user rows so they report the correct semantic event type.
-- Assistant rows created by earlier code remain 'token' (a single aggregated
-- token event) to preserve history.
UPDATE `messages` SET `event_type` = 'message' WHERE `role` = 'user';

-- The original covering index was on (session_id, msg_index). Because
-- msg_index only has meaning within a single turn, the read path now orders by
-- (msg_time, msg_index). The existing idx_messages_session_time index on
-- (session_id, msg_time) already supports the leading sort key.
