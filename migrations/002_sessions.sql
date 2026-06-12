-- 002_sessions.sql
-- blowball sessions table.
-- One row per conversation. Session lifecycle is owned by a user; deleting
-- the user cascades to their sessions, titles, and messages.

CREATE TABLE IF NOT EXISTS `sessions` (
    `session_id`  CHAR(36)  NOT NULL COMMENT 'UUID primary key',
    `user_id`     CHAR(36)  NOT NULL COMMENT 'Owning user (FK users.user_id)',
    `trace_id`    CHAR(36)  NOT NULL COMMENT 'request trace that created/last-touched the row',
    `update_time` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `create_time` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`session_id`),
    KEY `idx_sessions_user_update` (`user_id`, `update_time` DESC),
    CONSTRAINT `fk_sessions_user` FOREIGN KEY (`user_id`)
        REFERENCES `users` (`user_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='blowball chat sessions';
