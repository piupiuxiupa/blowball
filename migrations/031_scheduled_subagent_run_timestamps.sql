-- 031_scheduled_subagent_run_timestamps.sql
-- Complete the scheduled run schema introduced by 028 without rewriting an
-- already-deployed migration. Apply once with scheduled-run writers stopped.

ALTER TABLE `scheduled_subagent_runs`
    ADD COLUMN `create_time` TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    ADD COLUMN `update_time` TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3);

-- Existing runs retain their historical timing instead of the ALTER time.
UPDATE `scheduled_subagent_runs`
SET `create_time` = `started_at`,
    `update_time` = `finished_at`;
