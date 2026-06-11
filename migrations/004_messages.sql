-- 004_messages.sql
-- blowball messages table.
-- Append-only log of every turn in a session, tagged with the producing
-- agent and the OpenAI-style role. Ordered within a session by msg_index.

CREATE TABLE IF NOT EXISTS `messages` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT COMMENT 'Surrogate PK',
    `session_id`  CHAR(36)     NOT NULL COMMENT 'FK sessions.session_id',
    `msg_time`    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'When the message was produced',
    `agent`       VARCHAR(32)  NOT NULL COMMENT 'Confuse | Chongzhi | Liang',
    `msg_index`   INT          NOT NULL COMMENT 'Monotonic per-session sequence number',
    `role`        VARCHAR(16)  NOT NULL COMMENT 'user | assistant | tool',
    `content`     MEDIUMTEXT   NOT NULL COMMENT 'Message body (text or JSON for tool calls)',
    `trace_id`    CHAR(36)     NOT NULL COMMENT 'Request trace that produced this message',
    `update_time` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    KEY `idx_messages_session_index` (`session_id`, `msg_index`),
    KEY `idx_messages_session_time`  (`session_id`, `msg_time`),
    CONSTRAINT `fk_messages_session` FOREIGN KEY (`session_id`)
        REFERENCES `sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='blowball per-session message log';
