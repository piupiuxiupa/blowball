-- 001_users.sql
-- blowball users table.
-- Stores account credentials (bcrypt password hash) and lifecycle status.
-- Each row is keyed by a UUID user_id and carries a trace_id for observability.

CREATE TABLE IF NOT EXISTS `users` (
    `user_id`     CHAR(36)     NOT NULL COMMENT 'UUID primary key',
    `username`    VARCHAR(64)  NOT NULL COMMENT 'Unique login name',
    `password`    VARCHAR(255) NOT NULL COMMENT 'bcrypt password hash',
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active | disabled',
    `update_time` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `create_time` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`user_id`),
    UNIQUE KEY `uk_users_username` (`username`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='blowball user accounts';
