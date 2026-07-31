-- 010_turn_usage.sql
-- Per-chat-turn token-usage persistence (turn-cost-tracking capability).
--
-- Every completed (or interrupted) chat turn records one row here carrying the
-- full usage object emitted on the SSE done event — total plus the per-agent
-- breakdown (`by_agent`) — so historical cost, per-session aggregation and
-- future cost-guard rails have a durable source. The done event itself is
-- intentionally excluded from the messages log (usage is observability data,
-- not chat content), so this dedicated table is the authoritative home for it.
--
-- usage_json stores the authoritative usage object shape (see the
-- turn-cost-tracking spec): `{total:{...}, by_agent:{<agent>:{...}},
-- meta:{sub_agent_invocations:[...], parallel:bool}}`. The redundant
-- total_tokens column lets per-session cost be summed without parsing JSON:
--   SELECT SUM(total_tokens) FROM turn_usage WHERE session_id = ?
--
-- Rows cascade with session deletion (ON DELETE CASCADE on session_id), so a
-- purged session loses its cost history in lockstep — matching the existing
-- sessions/messages/titles cascade. No `*_deleted` mirror table is added here
-- (unlike 008_deletion_archive.sql): the design explicitly defers an audit-
-- grade archive until operators request "deleted-session cost lookup"; the
-- sessions/titles/messages mirrors already preserve the conversational audit
-- trail.
--
-- Style mirrors 004_messages.sql / 008_deletion_archive.sql: InnoDB, utf8mb4,
-- CHAR(36) UUID keys, TIMESTAMP(3) millisecond precision, named KEY indexes.

CREATE TABLE IF NOT EXISTS `turn_usage` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT COMMENT 'Surrogate PK',
    `session_id`   CHAR(36)     NOT NULL COMMENT 'FK sessions.session_id (cascades with session deletion)',
    `trace_id`     CHAR(36)     NOT NULL COMMENT 'Request trace that produced this turn (identifies a single request)',
    `user_id`      CHAR(36)     NOT NULL COMMENT 'Owning user (denormalized from sessions for cost queries without a join)',
    `usage_json`   MEDIUMTEXT   NOT NULL COMMENT 'Full usage object: {total, by_agent, meta} (authoritative shape)',
    `total_tokens` INT          NOT NULL COMMENT 'Redundant total_tokens from usage_json for fast SUM() aggregation',
    `created_at`   TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'When the turn completed (millisecond precision)',
    PRIMARY KEY (`id`),
    KEY `idx_turn_usage_session_time` (`session_id`, `created_at`),
    KEY `idx_turn_usage_trace` (`trace_id`),
    CONSTRAINT `fk_turn_usage_session` FOREIGN KEY (`session_id`)
        REFERENCES `sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Per-chat-turn per-agent token usage (turn-cost-tracking)';
