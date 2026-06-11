-- 003_titles.sql
-- blowball session titles table.
-- At most one auto-generated title per session (session_id is the PK).
-- Deleting the session cascades to its title row.

CREATE TABLE IF NOT EXISTS `titles` (
    `id`        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `session_id`  CHAR(36)     NOT NULL COMMENT 'UUID primary key, FK sessions.session_id',
    `title`       VARCHAR(128) NOT NULL COMMENT 'Short human-readable session title',
    `trace_id`    CHAR(36)     NOT NULL COMMENT 'request trace that created/last-touched the row',
    `update_time` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `create_time` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`session_id`),
    KEY idx_id (`id`),
    CONSTRAINT `fk_titles_session` FOREIGN KEY (`session_id`)
        REFERENCES `sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='blowball session display titles';
