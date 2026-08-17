-- 011_llm_raw_log.sql
-- Raw LLM request/response/error persistence (llm-raw-capture capability).
--
-- Every LLM call routed through OpenAIClient.StreamChat (Confucius /
-- Chongzhi / Liang loops, round-cap wrap-up rounds, title generation) leaves
-- its rows here: kind='request' captured when the as-sent params are built,
-- one kind='chunk' row per SSE frame (raw = the frame's wire bytes, verbatim),
-- and kind='response' (stitched non-streaming-equivalent) or kind='error'
-- (gateway status + raw body) when the stream ends. All rows of a call share
-- call_id and seq; frame_index orders them within the call, so
-- ORDER BY seq, frame_index replays a turn's LLM calls frame-by-frame.
-- Per-call chunk capture is capped (8MB cumulative) with an explicit
-- {"_truncated":true,...} marker row so a runaway stream cannot overflow
-- MEDIUMTEXT. Rows flow through a Redis write-behind buffer (llm_raw:buffer)
-- and are batch-inserted by a background flusher — never on the streaming
-- hot path (see internal/llmraw).
--
-- raw stores the payload verbatim: the exact ChatCompletionNewParams JSON as
-- sent (request), the stitched chat.completion-equivalent JSON (response), or
-- the gateway's raw error body (error). The model / finish_reason /
-- http_status / duration_ms columns are materialized redundantly from raw so
-- operators can filter without parsing JSON (same rationale as
-- turn_usage.total_tokens).
--
-- Deliberately NO foreign key to sessions and NO cleanup: rows survive
-- session deletion as orphans (post-mortem forensics for deleted sessions is
-- exactly the use case) and there is no retention job — growth is managed by
-- operator capacity/backup policy. This intentionally diverges from
-- turn_usage's ON DELETE CASCADE.
--
-- Style mirrors 010_turn_usage.sql: InnoDB, utf8mb4, CHAR(36) UUID keys,
-- TIMESTAMP(3) millisecond precision, named KEY indexes.

CREATE TABLE IF NOT EXISTS `llm_raw_log` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT COMMENT 'Surrogate PK (insert order)',
    `call_id`       CHAR(36)     NOT NULL COMMENT 'LLM call anchor: every row of one call (request/chunks/response/error) shares it',
    `seq`           INT          NOT NULL COMMENT 'Monotonic call sequence; ordering of calls within a trace',
    `frame_index`   INT          NOT NULL DEFAULT 0 COMMENT 'Record ordinal within the call: request=0, kind=chunk 1..N in arrival order, response/error last (ORDER BY seq, frame_index)',
    `trace_id`      CHAR(36)     NOT NULL COMMENT 'Request trace that produced the call',
    `session_id`    CHAR(36)     NOT NULL COMMENT 'Session the call belongs to (denormalized; intentionally no FK so rows survive session deletion)',
    `user_id`       CHAR(36)     NOT NULL COMMENT 'Owning user (denormalized for queries without a join)',
    `agent`         VARCHAR(32)  NOT NULL COMMENT 'Agent that made the call: Confucius | Chongzhi | Liang | title',
    `kind`          VARCHAR(16)  NOT NULL COMMENT 'request | chunk | response | error',
    `model`         VARCHAR(128) NOT NULL COMMENT 'Model name as requested',
    `finish_reason` VARCHAR(32)  NOT NULL DEFAULT '' COMMENT 'finish_reason from the response (redundant with raw)',
    `http_status`   SMALLINT     NOT NULL DEFAULT 0 COMMENT 'Gateway HTTP status on kind=error; 0 otherwise',
    `duration_ms`   INT          NOT NULL DEFAULT 0 COMMENT 'Stream duration from request-send to completion (redundant with raw)',
    `raw`           MEDIUMTEXT   NOT NULL COMMENT 'Raw payload: as-sent request JSON / per-frame wire-bytes chunk JSON / stitched response JSON / gateway error body',
    `msg_time`      TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'When the record was captured (millisecond precision)',
    `update_time`   TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT 'Row maintenance timestamp',
    PRIMARY KEY (`id`),
    KEY `idx_llm_raw_session_seq` (`session_id`, `seq`, `frame_index`),
    KEY `idx_llm_raw_trace_seq` (`trace_id`, `seq`),
    KEY `idx_llm_raw_time` (`msg_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Raw LLM request/response/error log (llm-raw-capture; no FK, never cleaned)';
