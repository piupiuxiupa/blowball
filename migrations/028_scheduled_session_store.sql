-- 028_scheduled_session_store.sql
-- Physically separate scheduled-task conversation storage
-- (extract-scheduler-service, scheduled-task-sessions capability).
--
-- Task executions reuse the existing turn engine but persist into this table
-- group instead of `sessions`/`messages`/`tool_calls`/... The group mirrors the
-- ordinary schema column-for-column (same idempotency keys, ordering cursor and
-- cascade semantics) so every canonical behavior — round-final transactions,
-- tool ordering, compaction anchors, sub-agent deltas — is preserved while the
-- ordinary chat surface can never observe task data.
--
-- Deliberate differences from the ordinary tables:
--   * scheduled_sessions carries the owning task/occurrence/run identity and a
--     UNIQUE occurrence_id (one session per occurrence; repeated preparation
--     reuses the same row).
--   * There is NO foreign key to `users`: occurrence/task facts and their
--     history must survive account lifecycle; the user-deletion flow archives
--     the content first and then removes it explicitly.
--   * All live child tables cascade from scheduled_sessions only. A deleted
--     scheduled session can therefore never leave orphan child rows, while the
--     execution facts in scheduled_task_runs (Scheduler-owned) survive.
--
-- The *_deleted mirrors follow the 008 archive convention (verbatim copy,
-- deletion_id + deleted_at, no foreign keys). As with the ordinary tables,
-- only the conversation-bearing rows are archived; tool_calls/usage/
-- compaction/sub-agent rows cascade away.
--
-- Existing databases must apply this migration manually.

CREATE TABLE IF NOT EXISTS `scheduled_sessions` (
    `session_id`        CHAR(36)  NOT NULL COMMENT 'UUID primary key, pre-allocated by the Scheduler',
    `user_id`           CHAR(36)  NOT NULL COMMENT 'Owning user (no FK so history survives account lifecycle)',
    `task_id`           CHAR(36)  NOT NULL COMMENT 'Owning scheduled task',
    `occurrence_id`     CHAR(36)  NOT NULL COMMENT 'Materialized occurrence; unique (one session per occurrence)',
    `run_id`            CHAR(36)  NOT NULL COMMENT 'Stable turn run identity for this occurrence',
    `trace_id`          CHAR(36)  NOT NULL COMMENT 'request trace that created/last-touched the row',
    `context_compacted` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '1 after the first context compaction; never reset',
    `update_time`       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `create_time`       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`session_id`),
    UNIQUE KEY `uk_scheduled_sessions_occurrence` (`occurrence_id`),
    UNIQUE KEY `uk_scheduled_sessions_run` (`run_id`),
    KEY `idx_scheduled_sessions_user_update` (`user_id`, `update_time` DESC),
    KEY `idx_scheduled_sessions_task_created` (`task_id`, `create_time` DESC, `session_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Scheduled-task conversation sessions (physically separate from sessions)';

CREATE TABLE IF NOT EXISTS `scheduled_messages` (
    `id`                BIGINT       NOT NULL AUTO_INCREMENT,
    `session_id`        CHAR(36)     NOT NULL COMMENT 'FK scheduled_sessions.session_id',
    `msg_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `agent`             VARCHAR(32)  NOT NULL,
    `msg_index`         INT          NOT NULL,
    `tool_call_id`      VARCHAR(64)  NULL COMMENT 'Placeholder anchor joining to scheduled_tool_calls',
    `role`              VARCHAR(16)  NULL,
    `event_type`        VARCHAR(16)  NOT NULL DEFAULT 'token',
    `status`            VARCHAR(16)  NULL COMMENT 'NULL normal; interrupted on truncated cancel/error finals',
    `content`           MEDIUMTEXT   NOT NULL,
    `attachments_json`  MEDIUMTEXT   NULL,
    `reasoning_content` MEDIUMTEXT   NULL,
    `trace_id`          CHAR(36)     NOT NULL,
    `client_msg_id`     CHAR(64)     NULL COMMENT 'Deterministic {trace_id}:{msg_index} idempotency key',
    `run_id`            CHAR(64)     NULL,
    `agent_instance_id` CHAR(24)     NULL,
    `update_time`       TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_scheduled_messages_client_msg_id` (`client_msg_id`),
    KEY `idx_scheduled_messages_session_index` (`session_id`, `msg_index`),
    KEY `idx_scheduled_messages_session_time` (`session_id`, `msg_time`),
    CONSTRAINT `fk_scheduled_messages_session` FOREIGN KEY (`session_id`)
        REFERENCES `scheduled_sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Scheduled-task message finals and structural rows';

CREATE TABLE IF NOT EXISTS `scheduled_tool_calls` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `session_id`   CHAR(36)     NOT NULL,
    `turn_id`      CHAR(36)     NOT NULL,
    `tool_call_id` VARCHAR(64)  NOT NULL,
    `name`         VARCHAR(128) NOT NULL,
    `arguments`    MEDIUMTEXT   NOT NULL,
    `result`       MEDIUMTEXT   NULL,
    `status`       VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `create_time`  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_scheduled_tool_calls_session_call` (`session_id`, `tool_call_id`),
    KEY `ix_scheduled_tool_calls_session_turn` (`session_id`, `turn_id`),
    CONSTRAINT `fk_scheduled_tool_calls_session` FOREIGN KEY (`session_id`)
        REFERENCES `scheduled_sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Scheduled-task canonical tool interactions (call+result paired)';

CREATE TABLE IF NOT EXISTS `scheduled_titles` (
    `session_id`  CHAR(36)     NOT NULL,
    `title`       VARCHAR(128) NOT NULL,
    `trace_id`    CHAR(36)     NOT NULL,
    `is_manual`   TINYINT(1)   NOT NULL DEFAULT 0,
    `update_time` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `create_time` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`session_id`),
    CONSTRAINT `fk_scheduled_titles_session` FOREIGN KEY (`session_id`)
        REFERENCES `scheduled_sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Scheduled-task session titles';

CREATE TABLE IF NOT EXISTS `scheduled_turn_usage` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `session_id`     CHAR(36)     NOT NULL,
    `trace_id`       CHAR(36)     NOT NULL,
    `user_id`        CHAR(36)     NOT NULL,
    `usage_json`     MEDIUMTEXT   NOT NULL,
    `total_tokens`   INT          NOT NULL,
    `model`          VARCHAR(64)  NULL,
    `context_tokens` INT          NOT NULL DEFAULT 0,
    `created_at`     TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_scheduled_turn_usage_session_time` (`session_id`, `created_at`),
    KEY `idx_scheduled_turn_usage_trace` (`trace_id`),
    CONSTRAINT `fk_scheduled_turn_usage_session` FOREIGN KEY (`session_id`)
        REFERENCES `scheduled_sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Scheduled-task per-turn token usage';

CREATE TABLE IF NOT EXISTS `scheduled_context_compactions` (
    `id`                        BIGINT       NOT NULL AUTO_INCREMENT,
    `session_id`                CHAR(36)     NOT NULL,
    `user_id`                   CHAR(36)     NOT NULL,
    `trace_id`                  CHAR(36)     NOT NULL,
    `trigger_kind`              VARCHAR(16)  NOT NULL,
    `trigger_tokens`            INT          NOT NULL,
    `content`                   MEDIUMTEXT   NOT NULL,
    `boundary_msg_time`         TIMESTAMP(3) NOT NULL,
    `boundary_msg_index`        INT          NOT NULL,
    `boundary_msg_id`           BIGINT       NOT NULL,
    `anchor_json`               MEDIUMTEXT   NULL,
    `anchor_truncated`          TINYINT(1)   NOT NULL DEFAULT 0,
    `shadowed_tokens`           INT          NOT NULL,
    `summary_model`             VARCHAR(128) NOT NULL,
    `summary_prompt_tokens`     INT          NOT NULL DEFAULT 0,
    `summary_completion_tokens` INT          NOT NULL DEFAULT 0,
    `retained_results_json`     MEDIUMTEXT   NULL,
    `measurement_basis`         VARCHAR(24)  NULL,
    `pre_compaction_tokens`     INT          NULL,
    `input_budget_tokens`       INT          NULL,
    `post_compaction_tokens`    INT          NULL,
    `retained_tail_messages`    INT          NULL,
    `summary_strategy`          VARCHAR(32)  NULL,
    `tool_summary_count`        INT          NULL,
    `tool_activity_count`       INT          NULL,
    `tool_activity_json`        MEDIUMTEXT   NULL,
    `create_time`               TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_scheduled_ctx_comp_session` (`session_id`, `id`),
    CONSTRAINT `fk_scheduled_ctx_comp_session` FOREIGN KEY (`session_id`)
        REFERENCES `scheduled_sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Scheduled-task append-only context-compaction checkpoints';

CREATE TABLE IF NOT EXISTS `scheduled_subagent_instances` (
    `id`                  BIGINT       NOT NULL AUTO_INCREMENT,
    `session_id`          CHAR(36)     NOT NULL,
    `agent_instance_id`   CHAR(24)     NOT NULL,
    `parent_instance_id`  CHAR(24)     NULL,
    `depth`               INT UNSIGNED NOT NULL,
    `name`                VARCHAR(128) NOT NULL,
    `system_prompt`       MEDIUMTEXT   NOT NULL,
    `tools_json`          JSON         NULL,
    `latest_run_id`       CHAR(64)     NULL,
    `create_time`         TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`         TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_scheduled_subagent_instances_session_instance` (`session_id`, `agent_instance_id`),
    KEY `idx_scheduled_subagent_instances_session_parent` (`session_id`, `parent_instance_id`),
    CONSTRAINT `fk_scheduled_subagent_instances_session` FOREIGN KEY (`session_id`)
        REFERENCES `scheduled_sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Scheduled-task dynamic sub-agent instance metadata';

CREATE TABLE IF NOT EXISTS `scheduled_subagent_runs` (
    `id`                     BIGINT       NOT NULL AUTO_INCREMENT,
    `session_id`             CHAR(36)     NOT NULL,
    `agent_instance_id`      CHAR(24)     NOT NULL,
    `run_id`                 CHAR(64)     NOT NULL,
    `previous_run_id`        CHAR(64)     NULL,
    `run_no`                 INT UNSIGNED NOT NULL DEFAULT 1,
    `parent_instance_id`     CHAR(24)     NULL,
    `depth`                  INT UNSIGNED NOT NULL,
    `name`                   VARCHAR(128) NOT NULL,
    `tools_json`             JSON         NULL,
    `status`                 VARCHAR(16)  NOT NULL,
    `snapshot_kind`          VARCHAR(16)  NOT NULL DEFAULT 'delta',
    `resume_eligible`        TINYINT(1)   NOT NULL DEFAULT 1,
    `base_message_count`     INT UNSIGNED NOT NULL DEFAULT 0,
    `message_count`          INT UNSIGNED NOT NULL DEFAULT 0,
    `context_message_count`  INT UNSIGNED NOT NULL DEFAULT 0,
    `context_bytes`          INT UNSIGNED NOT NULL DEFAULT 0,
    `messages_json`          JSON         NOT NULL,
    `started_at`             TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `finished_at`            TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_scheduled_subagent_runs_session_run` (`session_id`, `run_id`),
    UNIQUE KEY `uk_scheduled_subagent_runs_instance_run_no` (`session_id`, `agent_instance_id`, `run_no`),
    KEY `idx_scheduled_subagent_runs_session_instance_run_no` (`session_id`, `agent_instance_id`, `run_no`),
    CONSTRAINT `fk_scheduled_subagent_runs_session` FOREIGN KEY (`session_id`)
        REFERENCES `scheduled_sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Scheduled-task per-run sub-agent transcript deltas';

CREATE TABLE IF NOT EXISTS `scheduled_attachments` (
    `id`          CHAR(36)     NOT NULL,
    `user_id`     CHAR(36)     NOT NULL,
    `session_id`  CHAR(36)     NOT NULL COMMENT 'Scheduled sessions have no staged-upload state',
    `filename`    VARCHAR(255) NOT NULL,
    `mime`        VARCHAR(64)  NOT NULL,
    `size`        BIGINT       NOT NULL,
    `message_id`  BIGINT       NULL,
    `create_time` TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `ix_scheduled_attachments_session` (`session_id`),
    KEY `ix_scheduled_attachments_user_session` (`user_id`, `session_id`),
    CONSTRAINT `fk_scheduled_attachments_session` FOREIGN KEY (`session_id`)
        REFERENCES `scheduled_sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Scheduled-task chat image attachment registry';

CREATE TABLE IF NOT EXISTS `scheduled_attachments_deleted` (
    `id`          CHAR(36)     NOT NULL,
    `user_id`     CHAR(36)     NOT NULL,
    `session_id`  CHAR(36)     NOT NULL,
    `filename`    VARCHAR(255) NOT NULL,
    `mime`        VARCHAR(64)  NOT NULL,
    `size`        BIGINT       NOT NULL,
    `message_id`  BIGINT       NULL,
    `create_time` TIMESTAMP(3) NOT NULL,
    `deleted_at`  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `deletion_id` CHAR(36)     NOT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_scheduled_attachments_deleted_session` (`session_id`),
    KEY `idx_scheduled_attachments_deleted_deletion` (`deletion_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Verbatim archive of deleted scheduled_attachments rows';

-- Deletion archives. Only conversation-bearing rows are mirrored, matching the
-- ordinary-session archive convention; tool_calls/usage/compaction/sub-agent
-- rows cascade away with their session.

CREATE TABLE IF NOT EXISTS `scheduled_sessions_deleted` (
    `session_id`        CHAR(36)  NOT NULL,
    `user_id`           CHAR(36)  NOT NULL,
    `task_id`           CHAR(36)  NOT NULL,
    `occurrence_id`     CHAR(36)  NOT NULL,
    `run_id`            CHAR(36)  NOT NULL,
    `trace_id`          CHAR(36)  NOT NULL,
    `context_compacted` TINYINT(1) NOT NULL DEFAULT 0,
    `update_time`       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `create_time`       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `deleted_at`        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `deletion_id`       CHAR(36)  NOT NULL,
    PRIMARY KEY (`session_id`),
    KEY `idx_scheduled_sessions_deleted_user` (`user_id`),
    KEY `idx_scheduled_sessions_deleted_task` (`task_id`),
    KEY `idx_scheduled_sessions_deleted_deletion` (`deletion_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Verbatim archive of deleted scheduled_sessions rows';

CREATE TABLE IF NOT EXISTS `scheduled_messages_deleted` (
    `id`               BIGINT       NOT NULL COMMENT 'Original scheduled_messages.id (preserved)',
    `session_id`       CHAR(36)     NOT NULL,
    `msg_time`         TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `agent`            VARCHAR(32)  NOT NULL,
    `msg_index`        INT          NOT NULL,
    `role`             VARCHAR(16)  NULL,
    `event_type`       VARCHAR(16)  NOT NULL DEFAULT 'token',
    `content`          MEDIUMTEXT   NOT NULL,
    `attachments_json` MEDIUMTEXT   NULL,
    `trace_id`         CHAR(36)     NOT NULL,
    `update_time`      TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `deleted_at`       TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `deletion_id`      CHAR(36)     NOT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_scheduled_messages_deleted_session` (`session_id`),
    KEY `idx_scheduled_messages_deleted_deletion` (`deletion_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Verbatim archive of deleted scheduled_messages rows';

CREATE TABLE IF NOT EXISTS `scheduled_titles_deleted` (
    `session_id`  CHAR(36)     NOT NULL,
    `title`       VARCHAR(128) NOT NULL,
    `trace_id`    CHAR(36)     NOT NULL,
    `update_time` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `create_time` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `deleted_at`  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `deletion_id` CHAR(36)     NOT NULL,
    PRIMARY KEY (`session_id`),
    KEY `idx_scheduled_titles_deleted_deletion` (`deletion_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Verbatim archive of deleted scheduled_titles rows';
