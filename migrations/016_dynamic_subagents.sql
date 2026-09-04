-- Dynamic generic sub-agents (dynamic-subagents capability).
--
-- agent_instance_id separates two identities that previously both lived in
-- messages.run_id: the stable instance thread (which survives resume) and one
-- dispatch's run (the parent spawn tool_call id). Existing and top-level rows
-- remain NULL and retain the legacy "route by agent name" behavior.

ALTER TABLE `messages`
    ADD COLUMN `agent_instance_id` CHAR(24) NULL
        COMMENT 'Stable dynamic sub-agent instance identity; NULL on top-level and legacy rows' AFTER `run_id`;

-- Run-end authoritative chat snapshots. Event rows remain display/history data;
-- resume never reconstructs model context from them. Deleting a session removes
-- its snapshots with it.
CREATE TABLE IF NOT EXISTS `subagent_runs` (
    `id`                 BIGINT       NOT NULL AUTO_INCREMENT COMMENT 'Surrogate PK',
    `session_id`         CHAR(36)     NOT NULL COMMENT 'FK sessions.session_id',
    `agent_instance_id`  CHAR(24)     NOT NULL COMMENT 'Stable instance identity, reused across resume',
    `parent_instance_id` CHAR(24)     NULL COMMENT 'Parent dynamic instance; NULL for root dispatch',
    `depth`              INT UNSIGNED NOT NULL COMMENT 'Tree depth (root dispatch = 1)',
    `name`               VARCHAR(128) NOT NULL COMMENT 'Dynamic attribution label',
    `tools_json`         JSON         NULL COMMENT 'Effective tool names after subset narrowing',
    `status`             VARCHAR(16)  NOT NULL COMMENT 'completed | capped | error',
    `resume_eligible`    TINYINT(1)   NOT NULL DEFAULT 1 COMMENT '0 when a snapshot is missing/too large to resume',
    `messages_json`      JSON         NOT NULL COMMENT 'Full OpenAI-chat []Message snapshot at run end',
    `create_time`        TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `update_time`        TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_subagent_runs_session_instance` (`session_id`, `agent_instance_id`),
    CONSTRAINT `fk_subagent_runs_session` FOREIGN KEY (`session_id`)
        REFERENCES `sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Dynamic sub-agent instance history snapshots';
