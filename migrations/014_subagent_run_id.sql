-- 014_subagent_run_id.sql
-- Sub-agent invocation run identity (subagent-run-identity capability; see
-- openspec subagent-run-identity). When Confucius dispatches parallel
-- same-name invoke_* calls (e.g. 3x invoke_chongzhi), the sub-agent token /
-- reasoning / tool events interleave in arrival order. Historically the only
-- attribution was the agent display name, so MergeEvents glued adjacent
-- same-name fragments from DIFFERENT runs together and the frontend rendered
-- the interleaved runs as one garbled segment.
--
-- This migration adds messages.run_id: the id of the parent invoke_*
-- tool_call that spawned the producing Run. Every event of one invocation
-- carries the same id (injected as Meta.parent_tool_call_id by the dispatch
-- layer and copied into the row at persistence time), so any consumer can
-- regroup interleaved rows with GROUP BY (agent, run_id) while the physical
-- row order (msg_time, msg_index) and the deterministic client_msg_id
-- idempotency system stay untouched.
--
-- NULL semantics: NULL on non-sub-agent events (Confucius top-level rows,
-- user rows) and on all legacy rows written before this migration — reading
-- a NULL is exactly the pre-change "no identity" behavior.
--
-- Index evaluation: deliberately NO index is added. Every read path fetches
-- by session first (idx_messages_session_time) and groups by run in
-- application memory; no query filters by run_id alone. messages is the
-- highest-volume table, so an (session_id, run_id) index would add pure
-- write cost with no read benefit. Post-mortem queries scan one session's
-- rows through the existing covering index.
--
-- Existing databases must apply this migration manually (the docker-compose
-- initdb mount only runs on first volume init), mirroring migrations
-- 012/013.

ALTER TABLE `messages`
    ADD COLUMN `run_id` CHAR(64) NULL
        COMMENT 'Sub-agent invocation identity: the parent invoke_* tool_call id; NULL on non-sub-agent events and legacy rows' AFTER `client_msg_id`;
