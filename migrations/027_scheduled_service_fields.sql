-- 027_scheduled_service_fields.sql
-- Standalone-scheduler ownership fields (extract-scheduler-service).
--
-- This migration only ADDS columns and one index; it rewrites no ordinary
-- session/message data. It prepares the two scheduler-owned tables for:
--
--   1. Immutable task-resource references (resource_revision + manifest_hash)
--      fixed into every materialized occurrence.
--   2. Explicit history provenance: every pre-existing occurrence is marked
--      legacy_interactive because it wrote its conversation into an ordinary
--      `sessions` row. New occurrences are `scheduled` and live in the
--      physically separate table group created by migration 028.
--   3. The cross-occurrence execution hold: when Runtime cannot prove the
--      previous execution stopped, execution_hold_occurrence_id blocks every
--      later occurrence of the task. The column is server-owned and read-only
--      to task configuration updates.
--   4. The unique start grant persisted by AuthorizeStart (one grant per
--      occurrence, bound to the Runtime receipt generation).
--
-- The legacy `session.mode=existing` policy is stopped here: those tasks used
-- to append task turns to a real user chat. New and updated tasks may not use
-- it, and existing rows are paused with an explicit migration reason so the
-- user must opt into the per-occurrence session policy. Their past history is
-- left untouched (still readable through the legacy_interactive source).
--
-- Existing databases must apply this migration manually (the docker-compose
-- initdb mount only runs on first volume init).

ALTER TABLE `scheduled_tasks`
    ADD COLUMN `resource_revision` BIGINT UNSIGNED NOT NULL DEFAULT 0
        COMMENT 'Immutable published resource revision bound to this task; 0 = no resources'
        AFTER `reasoning_effort`,
    ADD COLUMN `manifest_hash` CHAR(64) NOT NULL DEFAULT ''
        COMMENT 'SHA-256 manifest digest of the bound resource revision'
        AFTER `resource_revision`,
    ADD COLUMN `execution_hold_occurrence_id` CHAR(36) NULL
        COMMENT 'Occurrence whose execution may still be running; blocks all later occurrences until a stop proof clears it'
        AFTER `paused_reason`,
    ADD COLUMN `blocked_reason` VARCHAR(64) NOT NULL DEFAULT ''
        COMMENT 'Server-owned read-only reason while execution_hold_occurrence_id is set'
        AFTER `execution_hold_occurrence_id`;

ALTER TABLE `scheduled_task_runs`
    ADD COLUMN `session_source` VARCHAR(32) NOT NULL DEFAULT 'scheduled'
        COMMENT 'scheduled = conversation lives in the scheduled table group; legacy_interactive = pre-split ordinary session'
        AFTER `session_mode`,
    ADD COLUMN `resource_revision` BIGINT UNSIGNED NOT NULL DEFAULT 0
        COMMENT 'Immutable pinned resource revision for this occurrence'
        AFTER `snapshot_json`,
    ADD COLUMN `manifest_hash` CHAR(64) NOT NULL DEFAULT ''
        COMMENT 'SHA-256 manifest digest pinned for this occurrence'
        AFTER `resource_revision`,
    ADD COLUMN `grant_generation` BIGINT UNSIGNED NOT NULL DEFAULT 0
        COMMENT 'Runtime receipt generation bound to the unique start grant; 0 = not granted'
        AFTER `next_attempt_at`,
    ADD COLUMN `grant_started_at` DATETIME(3) NULL
        COMMENT 'Logical start time fixed by the grant (authoritative for the Runtime deadline)'
        AFTER `grant_generation`,
    ADD COLUMN `grant_deadline` DATETIME(3) NULL
        COMMENT 'Fixed execution deadline computed at grant time'
        AFTER `grant_started_at`,
    ADD COLUMN `runtime_worker_generation` VARCHAR(64) NOT NULL DEFAULT ''
        COMMENT 'Runtime worker generation bound to the grant for fencing'
        AFTER `grant_deadline`,
    ADD COLUMN `terminal_receipt_generation` BIGINT UNSIGNED NOT NULL DEFAULT 0
        COMMENT 'Receipt generation whose verified terminal fact projected this occurrence'
        AFTER `finished_at`,
    ADD KEY `idx_scheduled_task_session_source` (`session_id`, `session_source`);

-- Every occurrence that existed before the split wrote into an ordinary
-- session. Mark the provenance explicitly; no row is moved or copied.
UPDATE `scheduled_task_runs` SET `session_source` = 'legacy_interactive';

-- Stop the retired session policy. `new` tasks keep running (their future
-- occurrences simply move to the scheduled table group); `existing` tasks are
-- paused and must be explicitly migrated by their owner.
UPDATE `scheduled_tasks`
SET `enabled` = 0,
    `paused_reason` = 'SESSION_POLICY_MIGRATION_REQUIRED',
    `next_run_at` = NULL
WHERE `session_mode` = 'existing'
  AND `deleted_at` IS NULL
  AND `enabled` = 1;
