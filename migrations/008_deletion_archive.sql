-- 008_deletion_archive.sql
-- Deletion archive mirror tables.
--
-- Deleting a session is irreversible (the live tables use ON DELETE CASCADE),
-- so before purging we copy the doomed rows verbatim into *_deleted mirror
-- tables. Each mirror reproduces its source table's columns byte-for-byte and
-- adds two audit columns:
--   deleted_at  — when the row was archived (the moment of the delete).
--   deletion_id — a UUID minted per delete operation; every row archived in the
--                 same delete shares this id, grouping a session with its
--                 titles and messages for later audit / restore.
--
-- Mirror tables carry NO foreign keys by design: an archive row must survive
-- even if its source user/session is later removed, and it must never be
-- cascade-deleted. messages_deleted.id is a plain BIGINT (not AUTO_INCREMENT)
-- so it can preserve the original messages.id value as its primary key.
--
-- users_deleted is created as scaffolding for a future user-deletion capability
-- and is NOT written by this change.

CREATE TABLE IF NOT EXISTS `messages_deleted` (
    `id`          BIGINT       NOT NULL COMMENT 'Original messages.id (preserved, not AUTO_INCREMENT)',
    `session_id`  CHAR(36)     NOT NULL COMMENT 'Original messages.session_id',
    `msg_time`    TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'When the message/event was produced (millisecond precision)',
    `agent`       VARCHAR(32)  NOT NULL COMMENT 'user | Confucius | Chongzhi | Liang; the producer of this row',
    `msg_index`   INT          NOT NULL COMMENT 'Per-turn sequence number (0 for user message, 1+ for assistant events)',
    `role`        VARCHAR(16)  NULL COMMENT 'OpenAI role (user/assistant/tool); NULL for marker events',
    `event_type`  VARCHAR(16)  NOT NULL DEFAULT 'token' COMMENT 'message | token | tool_call | agent_start | agent_end | agent_error | reasoning',
    `content`     MEDIUMTEXT   NOT NULL COMMENT 'Message body (text or JSON for tool calls)',
    `trace_id`    CHAR(36)     NOT NULL COMMENT 'Request trace that produced this message',
    `update_time` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Original update_time copied verbatim',
    `deleted_at`  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'When the row was archived',
    `deletion_id` CHAR(36)     NOT NULL COMMENT 'UUID grouping all rows from the same delete operation',
    PRIMARY KEY (`id`),
    KEY `idx_messages_deleted_session` (`session_id`),
    KEY `idx_messages_deleted_deletion` (`deletion_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Verbatim archive of deleted messages rows';

CREATE TABLE IF NOT EXISTS `sessions_deleted` (
    `session_id`  CHAR(36)  NOT NULL COMMENT 'Original sessions.session_id',
    `user_id`     CHAR(36)  NOT NULL COMMENT 'Original sessions.user_id',
    `trace_id`    CHAR(36)  NOT NULL COMMENT 'Original sessions.trace_id',
    `update_time` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Original update_time copied verbatim',
    `create_time` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Original create_time copied verbatim',
    `deleted_at`  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'When the row was archived',
    `deletion_id` CHAR(36)  NOT NULL COMMENT 'UUID grouping all rows from the same delete operation',
    PRIMARY KEY (`session_id`),
    KEY `idx_sessions_deleted_user` (`user_id`),
    KEY `idx_sessions_deleted_deletion` (`deletion_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Verbatim archive of deleted sessions rows';

CREATE TABLE IF NOT EXISTS `titles_deleted` (
    `session_id`  CHAR(36)     NOT NULL COMMENT 'Original titles.session_id',
    `title`       VARCHAR(128) NOT NULL COMMENT 'Short human-readable session title',
    `trace_id`    CHAR(36)     NOT NULL COMMENT 'Original titles.trace_id',
    `update_time` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Original update_time copied verbatim',
    `create_time` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Original create_time copied verbatim',
    `deleted_at`  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'When the row was archived',
    `deletion_id` CHAR(36)     NOT NULL COMMENT 'UUID grouping all rows from the same delete operation',
    PRIMARY KEY (`session_id`),
    KEY `idx_titles_deleted_deletion` (`deletion_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Verbatim archive of deleted titles rows';

CREATE TABLE IF NOT EXISTS `users_deleted` (
    `user_id`     CHAR(36)     NOT NULL COMMENT 'Original users.user_id',
    `username`    VARCHAR(64)  NOT NULL COMMENT 'Original users.username (NOT unique here: a username may be reused after deletion)',
    `password`    VARCHAR(255) NOT NULL COMMENT 'Original bcrypt password hash',
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active | disabled',
    `update_time` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Original update_time copied verbatim',
    `create_time` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Original create_time copied verbatim',
    `deleted_at`  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'When the row was archived',
    `deletion_id` CHAR(36)     NOT NULL COMMENT 'UUID grouping all rows from the same delete operation',
    PRIMARY KEY (`user_id`),
    KEY `idx_users_deleted_deletion` (`deletion_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Scaffolding archive for a future user-deletion capability; not written by this change';
