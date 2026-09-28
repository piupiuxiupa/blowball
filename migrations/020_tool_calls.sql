-- Final-content message persistence (refactor-message-storage).
--
-- messages gains final-row columns: reasoning_content holds the merged
-- reasoning of an assistant final (event_type='message'); status marks a
-- truncated final ('interrupted') written on the cancel/error path;
-- tool_call_id is the placeholder anchor — a tiny structural row marking one
-- tool interaction's position in the (msg_time, msg_index) stream, joined to
-- the canonical payload in tool_calls. All three stay NULL on legacy event
-- rows, which are never migrated.

ALTER TABLE `messages`
    ADD COLUMN `reasoning_content` MEDIUMTEXT NULL
        COMMENT 'Merged reasoning on assistant final rows; NULL on legacy event rows' AFTER `content`,
    ADD COLUMN `status` VARCHAR(16) NULL
        COMMENT 'NULL normal; interrupted on truncated cancel/error finals' AFTER `event_type`,
    ADD COLUMN `tool_call_id` VARCHAR(64) NULL
        COMMENT 'Placeholder anchor: joins this row to its tool_calls payload; set only on tool placeholder rows' AFTER `msg_index`;

-- Canonical tool-interaction store: one row pairs a call with its result.
-- tool_call_id is the OpenAI wire pairing key and the idempotency key (UK);
-- messages.tool_call_id placeholder rows locate each interaction in the
-- conversation stream — no ordinal columns needed.

CREATE TABLE IF NOT EXISTS `tool_calls` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT COMMENT 'Surrogate PK',
    `session_id`   CHAR(36)     NOT NULL COMMENT 'FK sessions.session_id',
    `turn_id`      CHAR(36)     NOT NULL COMMENT 'Producing turn identity (messages.trace_id)',
    `tool_call_id` VARCHAR(64)  NOT NULL COMMENT 'OpenAI tool_call pairing key',
    `name`         VARCHAR(128) NOT NULL COMMENT 'Tool/function name',
    `arguments`    MEDIUMTEXT   NOT NULL COMMENT 'Raw JSON arguments',
    `result`       MEDIUMTEXT   NULL COMMENT 'Raw result output (NULL until the result arrives)',
    `status`       VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending | completed',
    `create_time`  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_tool_calls_session_call` (`session_id`, `tool_call_id`),
    KEY `ix_tool_calls_session_turn` (`session_id`, `turn_id`),
    CONSTRAINT `fk_tool_calls_session` FOREIGN KEY (`session_id`)
        REFERENCES `sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Canonical tool interaction log (call+result paired)';
