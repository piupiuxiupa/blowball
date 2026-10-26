-- 024_context_compaction_anchor.sql
-- Persist a bounded projection of an oversized first-message anchor. NULL /
-- false keeps the original message verbatim; set records carry a deterministic
-- excerpt plus original-size/retrieval metadata.

ALTER TABLE `context_compactions`
    ADD COLUMN `anchor_content` MEDIUMTEXT NULL
        COMMENT 'Bounded first-message excerpt when the full anchor exceeds its budget' AFTER `boundary_msg_id`,
    ADD COLUMN `anchor_truncated` TINYINT(1) NOT NULL DEFAULT 0
        COMMENT '1 when anchor_content supersedes the original first message for model context' AFTER `anchor_content`;
