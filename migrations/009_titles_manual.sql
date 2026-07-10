-- 009_titles_manual.sql
-- Add is_manual flag to titles so user-edited titles are not overwritten by
-- asynchronous AI title generation.

ALTER TABLE `titles`
    ADD COLUMN `is_manual` BOOLEAN NOT NULL DEFAULT FALSE COMMENT 'TRUE when the title was manually set by the user; AI generation skips these rows' AFTER  `title`;
