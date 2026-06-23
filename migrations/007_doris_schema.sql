-- 007_doris_schema.sql
-- blowball schema for Apache Doris.
--
-- This file translates the MySQL migrations (001-006) into Doris-compatible
-- DDL. It is intended to be run against a Doris cluster instead of the
-- individual MySQL migration files.
--
-- Doris-specific adaptations:
--   - Uses UNIQUE KEY data model to preserve primary-key semantics.
--   - FOREIGN KEY constraints are removed (Doris does not enforce them).
--   - `ON UPDATE CURRENT_TIMESTAMP` is removed; the application should set
--     `update_time` on writes.
--   - `ENGINE=InnoDB` and `DEFAULT CHARSET=utf8mb4` are removed.
--   - Auto-increment `messages.id` is kept as the leading unique-key column
--     (required by Doris for auto-increment columns).
--   - Distribution keys are chosen to co-locate rows accessed together:
--     users by user_id, sessions/titles/messages by session_id.
--   - Secondary indexes are added for the query patterns used by the app.

-- ---------------------------------------------------------------------
-- users
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `users` (
    `user_id`     CHAR(36)     NOT NULL COMMENT 'UUID primary key',
    `username`    VARCHAR(64)  NOT NULL COMMENT 'Unique login name',
    `password`    VARCHAR(255) NOT NULL COMMENT 'bcrypt password hash',
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active | disabled',
    `update_time` DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Last update time (set by application)',
    `create_time` DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX `idx_users_username` (`username`)
)
UNIQUE KEY (`user_id`)
DISTRIBUTED BY HASH(`user_id`) BUCKETS 10
PROPERTIES (
    "replication_num" = "3"
);

-- ---------------------------------------------------------------------
-- sessions
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `sessions` (
    `session_id`  CHAR(36)  NOT NULL COMMENT 'UUID primary key',
    `user_id`     CHAR(36)  NOT NULL COMMENT 'Owning user (logical FK users.user_id)',
    `trace_id`    CHAR(36)  NOT NULL COMMENT 'request trace that created/last-touched the row',
    `update_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Last update time (set by application)',
    `create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX `idx_sessions_user_update` (`user_id`)
)
UNIQUE KEY (`session_id`)
DISTRIBUTED BY HASH(`session_id`) BUCKETS 10
PROPERTIES (
    "replication_num" = "3"
);

-- ---------------------------------------------------------------------
-- titles
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `titles` (
    `session_id`  CHAR(36)     NOT NULL COMMENT 'UUID primary key, logical FK sessions.session_id',
    `title`       VARCHAR(128) NOT NULL COMMENT 'Short human-readable session title',
    `trace_id`    CHAR(36)     NOT NULL COMMENT 'request trace that created/last-touched the row',
    `update_time` DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Last update time (set by application)',
    `create_time` DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP
)
UNIQUE KEY (`session_id`)
DISTRIBUTED BY HASH(`session_id`) BUCKETS 10
PROPERTIES (
    "replication_num" = "3"
);

-- ---------------------------------------------------------------------
-- messages
-- ---------------------------------------------------------------------
-- Note: Doris requires an auto-increment column to be the first column of
-- the unique key, hence the unique key order (id, session_id, msg_time).
-- Rows are still distributed by session_id so that a session's history is
-- co-located on the same backend node.
CREATE TABLE IF NOT EXISTS `messages` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT COMMENT 'Surrogate PK',
    `session_id`  CHAR(36)     NOT NULL COMMENT 'Logical FK sessions.session_id',
    `msg_time`    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'When the message/event was produced (millisecond precision)',
    `agent`       VARCHAR(32)  NOT NULL COMMENT 'user | Confuse | Chongzhi | Liang; the producer of this row',
    `msg_index`   INT          NOT NULL COMMENT 'Per-turn sequence number (0 for user message, 1+ for assistant events)',
    `role`        VARCHAR(16)  NULL     COMMENT 'OpenAI role (user/assistant/tool); NULL for marker events',
    `event_type`  VARCHAR(16)  NOT NULL DEFAULT 'token' COMMENT 'message | token | tool_call | agent_start | agent_end | agent_error | reasoning',
    `content`     STRING   NOT NULL COMMENT 'Message body (text or JSON for tool calls)',
    `trace_id`    CHAR(36)     NOT NULL COMMENT 'Request trace that produced this message',
    `update_time` DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Last update time (set by application)',
    INDEX `idx_messages_session_index` (`msg_index`),
    INDEX `idx_messages_session_time`  (`msg_time`)
)
UNIQUE KEY (`id`, `session_id`)
DISTRIBUTED BY HASH(`session_id`) BUCKETS 16
PROPERTIES (
    "replication_num" = "3"
);
