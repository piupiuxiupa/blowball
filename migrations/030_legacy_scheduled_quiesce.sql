-- 030_legacy_scheduled_quiesce.sql
-- Data-only quiesce of pre-split occurrences (extract-scheduler-service).
--
-- Apply this AFTER the old embedded scheduler has been stopped (see the
-- migration plan in docs/scheduled-tasks.md). It rewrites no ordinary session
-- data and adds no schema.
--
--   * Legacy pending/preparing rows and launchings whose startup fence was
--     never crossed (started_at IS NULL) are safe to cancel: no Runtime receipt
--     exists and no model call could have happened.
--   * Legacy running rows and launchings that crossed the fence are marked
--     interrupted/RESULT_UNKNOWN. The old scheduler cannot prove that its
--     Agent turn stopped, so the holding task receives a persistent
--     execution_hold_occurrence_id: every later occurrence is skipped
--     (TASK_EXECUTION_UNCERTAIN) until a controlled stop proof clears it. No
--     occurrence is replayed.
--   * History keeps session_source=legacy_interactive (set by 027); its content
--     stays in the ordinary sessions table and is never copied.
--
-- Existing databases must apply this migration manually.

UPDATE `scheduled_task_runs`
SET `status` = 'cancelled',
    `reason` = 'TASK_PAUSED',
    `claim_token` = '',
    `lease_until` = NULL,
    `next_attempt_at` = NULL,
    `finished_at` = UTC_TIMESTAMP(3),
    `update_time` = NOW()
WHERE `session_source` = 'legacy_interactive'
  AND (`status` IN ('pending', 'preparing')
       OR (`status` = 'launching' AND `started_at` IS NULL));

UPDATE `scheduled_task_runs`
SET `status` = 'interrupted',
    `reason` = 'RESULT_UNKNOWN',
    `claim_token` = '',
    `lease_until` = NULL,
    `next_attempt_at` = NULL,
    `finished_at` = UTC_TIMESTAMP(3),
    `update_time` = NOW()
WHERE `session_source` = 'legacy_interactive'
  AND (`status` = 'running'
       OR (`status` = 'launching' AND `started_at` IS NOT NULL));

-- Cross-occurrence hold: the latest interrupted legacy occurrence owns it.
UPDATE `scheduled_tasks` t
SET t.`execution_hold_occurrence_id` = (
        SELECT r.`occurrence_id` FROM `scheduled_task_runs` r
        WHERE r.`task_id` = t.`task_id`
          AND r.`session_source` = 'legacy_interactive'
          AND r.`status` = 'interrupted'
        ORDER BY r.`scheduled_for` DESC, r.`id` DESC
        LIMIT 1),
    t.`blocked_reason` = 'RESULT_UNKNOWN',
    t.`update_time` = NOW()
WHERE t.`deleted_at` IS NULL
  AND (t.`execution_hold_occurrence_id` IS NULL OR t.`execution_hold_occurrence_id` = '')
  AND EXISTS (
      SELECT 1 FROM `scheduled_task_runs` r2
      WHERE r2.`task_id` = t.`task_id`
        AND r2.`session_source` = 'legacy_interactive'
        AND r2.`status` = 'interrupted');
