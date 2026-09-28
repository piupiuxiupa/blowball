-- Final-content message persistence (refactor-message-storage).
--
-- messages gains final-row columns: reasoning_content holds the merged
-- reasoning of an assistant final (event_type='message'); status marks a
-- truncated final ('interrupted') written on the cancel/error path; round_no
-- is the 1-based round ordinal within the turn, the join key into tool_calls.
-- All three stay NULL on legacy event rows, which are never migrated.

ALTER TABLE `messages`
    ADD COLUMN `reasoning_content` MEDIUMTEXT NULL
        COMMENT 'Merged reasoning on assistant final rows; NULL on legacy event rows' AFTER `content`,
    ADD COLUMN `status` VARCHAR(16) NULL
        COMMENT 'NULL normal; interrupted on truncated cancel/error finals' AFTER `event_type`,
    ADD COLUMN `round_no` INT NULL
        COMMENT '1-based round ordinal within the turn; join key into tool_calls; NULL on legacy/user rows' AFTER `msg_index`;

-- Canonical tool-interaction store: one row pairs a call with its result.
-- tool_call_id is the OpenAI wire pairing key and the idempotency key (UK);
-- (turn_id, round_no) locates the interaction inside the conversation.

CREATE TABLE IF NOT EXISTS `tool_calls` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT COMMENT 'Surrogate PK',
    `session_id`   CHAR(36)     NOT NULL COMMENT 'FK sessions.session_id',
    `turn_id`      CHAR(36)     NOT NULL COMMENT 'Producing turn identity (messages.trace_id)',
    `round_no`     INT          NOT NULL COMMENT '1-based round ordinal within the turn',
    `tool_call_id` VARCHAR(64)  NOT NULL COMMENT 'OpenAI tool_call pairing key',
    `name`         VARCHAR(128) NOT NULL COMMENT 'Tool/function name',
    `arguments`    MEDIUMTEXT   NOT NULL COMMENT 'Raw JSON arguments',
    `result`       MEDIUMTEXT   NULL COMMENT 'Raw result output (NULL until the result arrives)',
    `status`       VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending | completed',
    `create_time`  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_tool_calls_session_call` (`session_id`, `tool_call_id`),
    KEY `ix_tool_calls_session_turn_round` (`session_id`, `turn_id`, `round_no`),
    CONSTRAINT `fk_tool_calls_session` FOREIGN KEY (`session_id`)
        REFERENCES `sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Canonical tool interaction log (call+result paired)';
