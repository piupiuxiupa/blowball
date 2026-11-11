-- 029_scheduled_execution_control.sql
-- Runtime execution facts and main-service resource-version metadata
-- (extract-scheduler-service, scheduled-task-service + scheduled-task-resources).
--
-- scheduled_execution_receipts is the authoritative execution record owned by
-- the main service. One row per occurrence:
--   * accepted            — SubmitScheduledRun transactionally created the
--                           receipt and the scheduled session. No model call
--                           may have happened yet.
--   * preparing           — a Runtime worker holds the CAS/lease and is doing
--                           read-only validation (owner, resources, admission).
--   * start_intent        — persisted BEFORE asking the Scheduler to authorize
--                           the start. From here on the execution may have had
--                           side effects; an unknown outcome is never replayed.
--   * running             — the unique start grant was received and the turn
--                           has begun; started_at is the grant's logical start.
--   * terminal            — one of succeeded/error/cancelled/interrupted/
--                           skipped; the final message/tool batch is durable
--                           before this fact is written.
--   * uncertain           — the execution may still be running (lost worker,
--                           crashed process, unreachable evidence). It remains
--                           capacity-accounted and blocks the task until a
--                           generation-bound stop proof exists.
-- stop_proof_* is written only by controlled reconciliation that positively
-- confirmed the original agent/managed commands stopped and fenced their
-- generation. Heartbeat loss, an expired lease, an expired deadline or "cancel
-- sent" never populate it.
--
-- Like the scheduled conversation group, receipts carry no FK: they must
-- survive scheduled-session deletion and Redis event retention.
--
-- Resource metadata:
--   * scheduled_resource_drafts holds the per-task draft revision/generation
--     used as the cross-API-node write fence (every draft mutation CAS-bumps
--     the revision; publication checks it has not changed).
--   * scheduled_resource_versions stores immutable publication metadata. The
--     manifest JSON is the canonical file list (relative path, size, SHA-256);
--     the bytes live under the shared workspace `.cron/<task_id>/revisions/`.
--     request_id is the idempotency key for a publication attempt.
--
-- Existing databases must apply this migration manually.

CREATE TABLE IF NOT EXISTS `scheduled_execution_receipts` (
    `occurrence_id`           CHAR(36)     NOT NULL COMMENT 'One execution receipt per occurrence',
    `task_id`                 CHAR(36)     NOT NULL,
    `user_id`                 CHAR(36)     NOT NULL,
    `run_id`                  CHAR(36)     NOT NULL COMMENT 'Stable turn run identity from the Scheduler',
    `session_id`              CHAR(36)     NOT NULL COMMENT 'Stable scheduled session identity',
    `attempt_count`           INT UNSIGNED NOT NULL COMMENT 'Scheduler attempt counter at acceptance',
    `config_fingerprint`      CHAR(64)     NOT NULL COMMENT 'Non-secret configuration fingerprint; identity, not credentials',
    `receipt_generation`      BIGINT UNSIGNED NOT NULL DEFAULT 1 COMMENT 'Execution identity generation; a new grant requires a new generation',
    `status`                  VARCHAR(16)  NOT NULL COMMENT 'accepted|preparing|start_intent|running|succeeded|error|cancelled|interrupted|skipped|uncertain',
    `reason`                  VARCHAR(64)  NOT NULL DEFAULT '',
    `last_error`              TEXT         NULL,
    `runtime_attempts`        INT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'Bounded pre-start preparation retries owned by the Runtime',
    `worker_generation`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'Runtime worker generation holding the lease',
    `lease_until`             DATETIME(3)  NULL,
    `accepted_at`             DATETIME(3)  NOT NULL,
    `start_intent_at`         DATETIME(3)  NULL,
    `started_at`              DATETIME(3)  NULL COMMENT 'Logical start fixed by the start grant',
    `finished_at`             DATETIME(3)  NULL,
    `grant_generation`        BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `grant_deadline`          DATETIME(3)  NULL,
    `stop_proof_present`      TINYINT(1)   NOT NULL DEFAULT 0,
    `stop_proof_generation`   BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `stop_proof_kind`         VARCHAR(32)  NOT NULL DEFAULT '',
    `stop_proof_at`           DATETIME(3)  NULL,
    `create_time`             TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`             TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`occurrence_id`),
    UNIQUE KEY `uk_scheduled_receipt_run` (`run_id`),
    UNIQUE KEY `uk_scheduled_receipt_session` (`session_id`),
    KEY `idx_scheduled_receipt_status` (`status`, `lease_until`, `accepted_at`),
    KEY `idx_scheduled_receipt_task` (`task_id`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Authoritative Runtime execution receipts (one per occurrence)';

CREATE TABLE IF NOT EXISTS `scheduled_resource_drafts` (
    `task_id`        CHAR(36)     NOT NULL,
    `user_id`        CHAR(36)     NOT NULL,
    `draft_revision` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'CAS fence revision; bumped by every authorized draft mutation',
    `generation`     CHAR(64)     NOT NULL DEFAULT '' COMMENT 'Opaque generation value used to detect concurrent publication',
    `update_time`    TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `create_time`    TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`task_id`),
    KEY `idx_scheduled_resource_drafts_owner` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Per-task `.cron` draft write fence';

CREATE TABLE IF NOT EXISTS `scheduled_resource_versions` (
    `task_id`           CHAR(36)     NOT NULL,
    `resource_revision` BIGINT UNSIGNED NOT NULL,
    `user_id`           CHAR(36)     NOT NULL,
    `manifest_hash`     CHAR(64)     NOT NULL COMMENT 'SHA-256 over the sorted manifest entries',
    `file_count`        INT UNSIGNED NOT NULL,
    `total_bytes`       BIGINT UNSIGNED NOT NULL,
    `manifest_json`     MEDIUMTEXT   NOT NULL COMMENT 'Sorted JSON [{path,size,sha256}] of actual copied bytes',
    `request_id`        CHAR(64)     NOT NULL COMMENT 'Publication idempotency key',
    `create_time`       TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`task_id`, `resource_revision`),
    UNIQUE KEY `uk_scheduled_resource_request` (`task_id`, `request_id`),
    KEY `idx_scheduled_resource_versions_owner` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Immutable published task resource versions';
