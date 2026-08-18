-- 013_context_compaction.sql
-- Context-compaction capability storage (see openspec
-- add-context-compaction): long sessions condense their middle history into a
-- structured checkpoint summary when context pressure reaches 80% of
-- openai.max_context_tokens, so the model context never grows unbounded into
-- the hard provider window limit.
--
-- Three additions:
--
--   1. context_compactions — append-only, latest-wins. Every completed
--      compaction inserts one row; stitching reads the (session_id, id)-max
--      row and historical rows remain as an audit trail. No
--      UNIQUE(session_id)+UPDATE churn: re-compaction simply supersedes.
--      `content` stores the BARE summary text — the <compacted-summary>
--      framing is applied at stitch time so the wire shape stays a
--      prompt-engineering concern, not a storage one.
--      The boundary is the composite cursor (msg_time, msg_index, id) of the
--      LAST shadowed persistence row — the same global ordering the messages
--      pagination cursor uses, so concurrent interleaved inserts can never
--      make a bare id diverge from logical order.
--      Observability columns: trigger_kind (mid_turn | turn_start),
--      trigger_tokens (measured pressure at trigger), shadowed_tokens (the
--      summary call's prompt size — a faithful proxy for the shadowed span),
--      and summary_model / summary_prompt_tokens / summary_completion_tokens
--      (the compaction LLM call's cost; it is deliberately NOT folded into
--      turn_usage — it is an operational call, not chat content).
--      FK ON DELETE CASCADE follows the turn_usage precedent: compaction
--      records are session-private business data and are NOT copied into the
--      008 deletion-archive mirrors (deep observability of the summary call
--      lives in llm_raw_log, which survives deletion).
--
--   2. sessions.context_compacted — the stitching gate. Set to 1 only AFTER
--      the record insert and the Redis cache write (crash-consistency write
--      order); never reset to 0. A crash before the flag leaves a harmless
--      orphan record superseded by the next compaction.
--
--   3. turn_usage.context_tokens — the LAST LLM round's prompt+completion of
--      each completed turn: the authoritative end-of-turn context size that
--      the turn-start preventive compaction check reads (the cumulative
--      total_tokens column can never serve — it sums every round).
--
--   4. messages.client_msg_id widened CHAR(36) → CHAR(64). The mid-turn flush
--      persists a turn's messages TWICE (flush + turn-end save) and dedupes
--      them through deterministic ids of the form {trace_id}:{msg_index};
--      a UUID trace_id alone is already 36 chars, so the composed id overflows
--      the CHAR(36) the write-behind migration 012 minted for bare UUIDs.
--      Widening keeps the UNIQUE index semantics; existing rows (UUID or
--      NULL) are untouched.
--
-- All new columns carry defaults so existing rows keep the exact pre-change
-- behavior; the capability is wholly inert while openai.max_context_tokens is
-- unset (0). Existing databases must apply this migration manually (the
-- docker-compose initdb mount only runs on first volume init).
--
-- Style mirrors 010_turn_usage.sql / 011_llm_raw_log.sql: InnoDB, utf8mb4,
-- named KEY indexes, millisecond-precision timestamps.

ALTER TABLE `sessions`
    ADD COLUMN `context_compacted` TINYINT(1) NOT NULL DEFAULT 0
        COMMENT '1 after the first context compaction completed; never reset (stitching gate)' AFTER `trace_id`;

ALTER TABLE `turn_usage`
    ADD COLUMN `context_tokens` INT NOT NULL DEFAULT 0
        COMMENT 'Last LLM round prompt+completion of the turn (authoritative end-of-turn context size for the turn-start compaction check)' AFTER `total_tokens`;

ALTER TABLE `messages`
    MODIFY COLUMN `client_msg_id` CHAR(64) NULL
        COMMENT 'Idempotency key: write-behind UUIDs (legacy) or deterministic {trace_id}:{msg_index} for mid-turn-flushed turns; NULL on legacy rows';

CREATE TABLE IF NOT EXISTS `context_compactions` (
    `id`                        BIGINT       NOT NULL AUTO_INCREMENT COMMENT 'Surrogate PK; (session_id, id) max row is the stitching source (latest-wins)',
    `session_id`                CHAR(36)     NOT NULL COMMENT 'FK sessions.session_id (cascades with session deletion)',
    `user_id`                   CHAR(36)     NOT NULL COMMENT 'Owning user (denormalized for queries without a join)',
    `trace_id`                  CHAR(36)     NOT NULL COMMENT 'Request trace that produced the compaction',
    `trigger_kind`              VARCHAR(16)  NOT NULL COMMENT 'mid_turn | turn_start',
    `trigger_tokens`            INT          NOT NULL COMMENT 'Measured context pressure (prompt+completion of the last round) at trigger',
    `content`                   MEDIUMTEXT   NOT NULL COMMENT 'Bare checkpoint summary text; the <compacted-summary> framing wraps it at stitch time',
    `boundary_msg_time`         TIMESTAMP(3) NOT NULL COMMENT 'Composite cursor: msg_time of the last shadowed messages row',
    `boundary_msg_index`        INT          NOT NULL COMMENT 'Composite cursor: msg_index of the last shadowed messages row',
    `boundary_msg_id`           BIGINT       NOT NULL COMMENT 'Composite cursor: id of the last shadowed messages row',
    `shadowed_tokens`           INT          NOT NULL COMMENT 'Approximate size of the shadowed span (the summary call prompt size)',
    `summary_model`             VARCHAR(128) NOT NULL COMMENT 'Model that generated the summary (the orchestrating agent model)',
    `summary_prompt_tokens`     INT          NOT NULL DEFAULT 0 COMMENT 'Summary LLM call prompt tokens',
    `summary_completion_tokens` INT          NOT NULL DEFAULT 0 COMMENT 'Summary LLM call completion tokens',
    `create_time`               TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'When the compaction completed (millisecond precision)',
    PRIMARY KEY (`id`),
    KEY `idx_ctx_comp_session` (`session_id`, `id`),
    CONSTRAINT `fk_ctx_comp_session` FOREIGN KEY (`session_id`)
        REFERENCES `sessions` (`session_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Append-only context-compaction checkpoint records (latest row wins for stitching)';
