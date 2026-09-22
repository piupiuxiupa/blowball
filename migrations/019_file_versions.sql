-- Turn artifact version index (turn-artifacts capability).
--
-- One row per artifact version snapshot: at each turn's end the backend
-- snapshots the turn's deliverables into a server-side blob store (outside
-- the user workspace) and records one row here per (path, version_id).
-- version_id is a UUID v7 (time-ordered), so as-of resolution is an ORDER BY.
-- Content bytes live on disk; this table is only the index.

CREATE TABLE IF NOT EXISTS `file_versions` (
    `id`          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `user_id`     VARCHAR(64)  NOT NULL COMMENT 'users.user_id (no FK; PK isolation suffices)',
    `path`        VARCHAR(1024) NOT NULL COMMENT 'Workspace-relative slash-separated path of the artifact',
    `version_id`  VARCHAR(64)  NOT NULL COMMENT 'UUID v7; blob address within the version store',
    `size`        BIGINT       NOT NULL COMMENT 'Snapshot size in bytes',
    `mime`        VARCHAR(255) NOT NULL DEFAULT '' COMMENT 'MIME type inferred at snapshot time',
    `sha256`      CHAR(64)     NOT NULL COMMENT 'Hex content digest; dedups identical consecutive snapshots',
    `create_time` TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_path_version` (`user_id`, `path`(255), `version_id`),
    KEY `idx_user_path_time` (`user_id`, `path`(255), `create_time`),
    UNIQUE KEY `uk_version_id` (`version_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Per-turn artifact version index';
