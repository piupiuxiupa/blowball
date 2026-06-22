-- 006_messages_reasoning.sql
-- Reasoning/thinking content is stored in the existing `content` column with
-- `event_type='reasoning'`. No separate column is required.
-- If an earlier version of this migration created a `reasoning_content` column,
-- drop it so the schema stays aligned with the model.

ALTER TABLE `messages` DROP COLUMN IF EXISTS `reasoning_content`;
