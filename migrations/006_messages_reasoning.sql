-- 006_messages_reasoning.sql
-- Reasoning/thinking content is stored in the existing `content` column with
-- `event_type='reasoning'`. No separate column is required.
-- If an earlier version of this migration created a `reasoning_content` column,
-- drop it so the schema stays aligned with the model.
--
-- The drop is guarded via information_schema + prepared statement because
-- MySQL (unlike MariaDB) has no `DROP COLUMN IF EXISTS`: on a fresh volume the
-- column never exists and this is a no-op; on a legacy database that ran the
-- earlier version, the column is dropped.

SET @col_exists = (
  SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 'messages'
    AND COLUMN_NAME = 'reasoning_content'
);
SET @ddl = IF(@col_exists > 0,
  'ALTER TABLE `messages` DROP COLUMN `reasoning_content`',
  'SELECT 1');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
