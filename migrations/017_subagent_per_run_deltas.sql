-- Sub-agent per-run transcript snapshots.
--
-- subagent_instances holds stable thread metadata. subagent_runs becomes a
-- per-dispatch ledger: each row stores only the messages added by that run.
-- Rows written by the previous instance-latest design remain explicitly marked
-- as legacy full snapshots and act as the baseline for later delta runs.

CREATE TABLE IF NOT EXISTS `subagent_instances` (
    `id`                BIGINT       NOT NULL AUTO_INCREMENT COMMENT 'Surrogate PK',
    `session_id`        CHAR(36)     NOT NULL COMMENT 'FK sessions.session_id',
    `agent_instance_id` CHAR(24)     NOT NULL COMMENT 'Stable dynamic sub-agent instance identity',
    `parent_instance_id` CHAR(24)    NULL COMMENT 'Parent dynamic instance; NULL for root dispatch',
    `depth`             INT UNSIGNED NOT NULL COMMENT 'Tree depth (root dispatch = 1)',
    `name`              VARCHAR(128) NOT NULL COMMENT 'Stable dynamic attribution label',
    `system_prompt`     MEDIUMTEXT   NOT NULL COMMENT 'Authoritative system prompt for the instance',
    `tools_json`        JSON         NULL COMMENT 'Effective tool names after subset narrowing',
    `latest_run_id`     CHAR(64)     NULL COMMENT 'Linear chain head used by resume',
    `create_time`       TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`       TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_subagent_instances_session_instance` (`session_id`, `agent_instance_id`),
    KEY `idx_subagent_instances_session_parent` (`session_id`, `parent_instance_id`),
    CONSTRAINT `fk_subagent_instances_session`
        FOREIGN KEY (`session_id`) REFERENCES `sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Dynamic sub-agent stable instance metadata';

ALTER TABLE `subagent_runs`
    ADD COLUMN `run_id` CHAR(64) NULL
        COMMENT 'Sub-agent invocation identity; parent spawn tool_call id'
        AFTER `agent_instance_id`,
    ADD COLUMN `previous_run_id` CHAR(64) NULL
        COMMENT 'Previous run when this row resumed an instance'
        AFTER `run_id`,
    ADD COLUMN `run_no` INT UNSIGNED NOT NULL DEFAULT 1
        COMMENT 'Linear execution number within the instance; starts at 1'
        AFTER `previous_run_id`,
    ADD COLUMN `snapshot_kind` VARCHAR(16) NOT NULL DEFAULT 'legacy_full'
        COMMENT 'legacy_full | delta'
        AFTER `status`,
    ADD COLUMN `base_message_count` INT UNSIGNED NOT NULL DEFAULT 0
        COMMENT 'Non-system messages present before this run started'
        AFTER `resume_eligible`,
    ADD COLUMN `message_count` INT UNSIGNED NOT NULL DEFAULT 0
        COMMENT 'Messages in this run delta'
        AFTER `base_message_count`,
    ADD COLUMN `context_message_count` INT UNSIGNED NOT NULL DEFAULT 0
        COMMENT 'Total non-system messages after appending this run'
        AFTER `message_count`,
    ADD COLUMN `context_bytes` INT UNSIGNED NOT NULL DEFAULT 0
        COMMENT 'Serialized complete system + stitched-context size'
        AFTER `context_message_count`,
    ADD COLUMN `started_at` TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
        COMMENT 'Dispatch start time',
    ADD COLUMN `finished_at` TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
        COMMENT 'Terminal run write time',
    ADD KEY `idx_subagent_runs_session_instance_run_no`
        (`session_id`, `agent_instance_id`, `run_no`);

-- Recover the last persisted run identity where possible. Old rows without a
-- matching event receive a deterministic legacy identity; no historical runs
-- are fabricated.
UPDATE `subagent_runs` sr
SET sr.`run_id` = COALESCE((
    SELECT m.`run_id`
    FROM `messages` m
    WHERE m.`session_id` = sr.`session_id`
      AND m.`agent_instance_id` = sr.`agent_instance_id`
      AND m.`run_id` IS NOT NULL
      AND m.`run_id` <> ''
    ORDER BY m.`id` DESC
    LIMIT 1
), CONCAT('legacy:', sr.`agent_instance_id`))
WHERE sr.`run_id` IS NULL OR sr.`run_id` = '';

UPDATE `subagent_runs`
SET `snapshot_kind` = 'legacy_full',
    `base_message_count` = 0,
    `message_count` = CAST(JSON_LENGTH(`messages_json`) AS UNSIGNED),
    `context_message_count` = CASE
        WHEN JSON_LENGTH(`messages_json`) > 0
         AND JSON_UNQUOTE(JSON_EXTRACT(`messages_json`, '$[0].role')) = 'system'
        THEN CAST(JSON_LENGTH(`messages_json`) - 1 AS UNSIGNED)
        ELSE CAST(JSON_LENGTH(`messages_json`) AS UNSIGNED)
    END,
    `context_bytes` = COALESCE(LENGTH(`messages_json`), 0),
    `started_at` = `create_time`,
    `finished_at` = `update_time`
WHERE `snapshot_kind` = 'legacy_full'
  AND `message_count` = 0;

INSERT INTO `subagent_instances` (
    `session_id`, `agent_instance_id`, `parent_instance_id`, `depth`, `name`,
    `system_prompt`, `tools_json`, `latest_run_id`
)
SELECT
    sr.`session_id`,
    sr.`agent_instance_id`,
    sr.`parent_instance_id`,
    sr.`depth`,
    sr.`name`,
    CASE
        WHEN JSON_LENGTH(sr.`messages_json`) > 0
         AND JSON_UNQUOTE(JSON_EXTRACT(sr.`messages_json`, '$[0].role')) = 'system'
        THEN COALESCE(JSON_UNQUOTE(JSON_EXTRACT(sr.`messages_json`, '$[0].content')), '')
        ELSE ''
    END,
    sr.`tools_json`,
    sr.`run_id`
FROM `subagent_runs` sr
WHERE NOT EXISTS (
    SELECT 1 FROM `subagent_instances` si
    WHERE si.`session_id` = sr.`session_id`
      AND si.`agent_instance_id` = sr.`agent_instance_id`
);

ALTER TABLE `subagent_runs`
    MODIFY `run_id` CHAR(64) NOT NULL,
    DROP KEY `uk_subagent_runs_session_instance`,
    ADD UNIQUE KEY `uk_subagent_runs_session_run` (`session_id`, `run_id`),
    ADD UNIQUE KEY `uk_subagent_runs_instance_run_no`
        (`session_id`, `agent_instance_id`, `run_no`);
