-- Image attachments on user messages (add-image-attachments).
--
-- attachments is the file registry: one row per uploaded chat image, owning
-- the id/user/session triple used for send-time ownership validation and the
-- session-delete lifecycle. The on-disk bytes live under
-- data/{user_id}/attachments/{session_id}/{id}.{ext}; base64 NEVER enters any
-- persistent tier — it is computed at request time from the immutable file.
-- message_id is a best-effort backfill reserved for orphan-cleanup stats
-- (uploaded but never sent); the authoritative message↔attachment linkage is
-- messages.attachments_json, which rides the message row through the same
-- write-behind path (one durability system, no split-brain).

CREATE TABLE IF NOT EXISTS `attachments` (
    `id`          CHAR(36)     NOT NULL COMMENT 'UUIDv7; also the on-disk filename stem',
    `user_id`     CHAR(36)     NOT NULL COMMENT 'Owning user (from the verified JWT)',
    `session_id`  CHAR(36)     NOT NULL COMMENT 'Owning session; directory-scoped lifecycle',
    `filename`    VARCHAR(255) NOT NULL COMMENT 'Original client filename, display only (never used in paths)',
    `mime`        VARCHAR(64)  NOT NULL COMMENT 'MIME sniffed from the file header magic bytes',
    `size`        BIGINT       NOT NULL COMMENT 'File size in bytes',
    `message_id`  BIGINT       NULL COMMENT 'Best-effort send linkage for orphan cleanup; NULL until backfilled',
    `create_time` TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `ix_attachments_session` (`session_id`),
    KEY `ix_attachments_user_session` (`user_id`, `session_id`),
    CONSTRAINT `fk_attachments_session` FOREIGN KEY (`session_id`)
        REFERENCES `sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Chat image attachment registry (files under data/{user}/attachments/{session})';

-- Deletion-archive mirror, same audit convention as the other *_deleted
-- tables (deletion_id + deleted_at stamped per delete operation).
CREATE TABLE IF NOT EXISTS `attachments_deleted` (
    `id`          CHAR(36)     NOT NULL COMMENT 'Original attachments.id',
    `user_id`     CHAR(36)     NOT NULL COMMENT 'Original attachments.user_id',
    `session_id`  CHAR(36)     NOT NULL COMMENT 'Original attachments.session_id',
    `filename`    VARCHAR(255) NOT NULL COMMENT 'Original attachments.filename',
    `mime`        VARCHAR(64)  NOT NULL COMMENT 'Original attachments.mime',
    `size`        BIGINT       NOT NULL COMMENT 'Original attachments.size',
    `message_id`  BIGINT       NULL COMMENT 'Original attachments.message_id',
    `create_time` TIMESTAMP(3) NOT NULL COMMENT 'Original create_time copied verbatim',
    `deleted_at`  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'When the row was archived',
    `deletion_id` CHAR(36)     NOT NULL COMMENT 'UUID grouping all rows from the same delete operation',
    PRIMARY KEY (`id`),
    KEY `idx_attachments_deleted_session` (`session_id`),
    KEY `idx_attachments_deleted_deletion` (`deletion_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Verbatim archive of deleted attachments rows';

-- Per-message attachment references. NULL on all pre-existing rows (no
-- attachments = plain text message, the exact pre-change semantics).
ALTER TABLE `messages`
    ADD COLUMN `attachments_json` MEDIUMTEXT NULL
        COMMENT 'JSON array [{id, filename, mime}] of image attachment references' AFTER `content`;

ALTER TABLE `messages_deleted`
    ADD COLUMN `attachments_json` MEDIUMTEXT NULL
        COMMENT 'Original messages.attachments_json copied verbatim' AFTER `content`;
