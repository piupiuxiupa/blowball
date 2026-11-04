-- 026_scheduled_agent_tasks.sql
-- Persistent user schedules and occurrence/execution history. Absolute slot and
-- lease values are UTC DATETIME(3). No JWT or LLM credential columns exist.

CREATE TABLE IF NOT EXISTS `scheduled_tasks` (
    `task_id`          CHAR(36)      NOT NULL COMMENT 'UUID task identity',
    `user_id`          CHAR(36)      NOT NULL COMMENT 'Owner identity; no FK so history survives account lifecycle',
    `name`             VARCHAR(255)  NOT NULL,
    `prompt`           MEDIUMTEXT    NOT NULL,
    `schedule_type`    VARCHAR(8)    NOT NULL COMMENT 'at | cron',
    `run_at`           DATETIME(3)   NULL COMMENT 'UTC one-shot instant',
    `cron_expression`  VARCHAR(128)  NOT NULL DEFAULT '',
    `timezone`         VARCHAR(64)   NOT NULL COMMENT 'IANA location',
    `session_mode`     VARCHAR(16)   NOT NULL DEFAULT 'new' COMMENT 'new | existing',
    `session_id`       CHAR(36)      NULL COMMENT 'Existing target; NULL for new-per-occurrence',
    `model`            VARCHAR(64)   NOT NULL DEFAULT '' COMMENT 'Empty = deployment default',
    `reasoning_effort` VARCHAR(16)   NOT NULL DEFAULT '' COMMENT 'Empty = deployment default',
    `enabled`          TINYINT(1)    NOT NULL DEFAULT 1,
    `next_run_at`      DATETIME(3)   NULL COMMENT 'Next unmaterialized UTC slot',
    `revision`         BIGINT UNSIGNED NOT NULL DEFAULT 1,
    `paused_reason`    VARCHAR(64)   NOT NULL DEFAULT '',
    `deleted_at`       DATETIME(3)   NULL COMMENT 'Soft-delete marker',
    `update_time`      TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `create_time`      TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`task_id`),
    KEY `idx_scheduled_tasks_due` (`deleted_at`, `enabled`, `next_run_at`),
    KEY `idx_scheduled_tasks_owner_history` (`user_id`, `deleted_at`, `create_time` DESC, `task_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='User-owned scheduled agent task definitions';

CREATE TABLE IF NOT EXISTS `scheduled_task_runs` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `occurrence_id`   CHAR(36)     NOT NULL COMMENT 'UUID occurrence identity',
    `task_id`         CHAR(36)     NOT NULL,
    `user_id`         CHAR(36)     NOT NULL COMMENT 'Denormalized owner for scoped history/recovery',
    `revision`        BIGINT UNSIGNED NOT NULL,
    `snapshot_json`   MEDIUMTEXT   NOT NULL COMMENT 'Immutable non-secret execution configuration',
    `scheduled_for`   DATETIME(3)  NOT NULL COMMENT 'UTC planned slot',
    `run_id`          CHAR(36)     NOT NULL COMMENT 'Stable turn Run identity',
    `session_id`      CHAR(36)     NOT NULL COMMENT 'Reserved new session or existing target',
    `session_mode`    VARCHAR(16)  NOT NULL COMMENT 'Immutable snapshot session mode: new | existing',
    `status`          VARCHAR(16)  NOT NULL COMMENT 'pending|preparing|launching|running|succeeded|error|skipped|cancelled|interrupted',
    `reason`          VARCHAR(64)  NOT NULL DEFAULT '',
    `claim_token`     CHAR(36)     NOT NULL DEFAULT '' COMMENT 'Lease/fence token; never a user credential',
    `lease_until`     DATETIME(3)  NULL,
    `attempt_count`   INT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'Persistent safe-not-started attempts',
    `next_attempt_at` DATETIME(3)  NULL,
    `started_at`      DATETIME(3)  NULL COMMENT 'Startup fence crossing time',
    `finished_at`     DATETIME(3)  NULL,
    `last_error`      TEXT         NULL,
    `update_time`     TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `create_time`     TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_scheduled_task_occurrence` (`occurrence_id`),
    UNIQUE KEY `uk_scheduled_task_slot` (`task_id`, `scheduled_for`),
    UNIQUE KEY `uk_scheduled_task_run_id` (`run_id`),
    KEY `idx_scheduled_task_due_lease` (`status`, `next_attempt_at`, `lease_until`),
    KEY `idx_scheduled_task_history` (`task_id`, `scheduled_for` DESC, `id` DESC),
    KEY `idx_scheduled_task_session_status` (`session_id`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Durable scheduled occurrences and execution history';
