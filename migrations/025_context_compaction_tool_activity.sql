-- 025_context_compaction_tool_activity.sql
-- Persist the deterministic checkpoint tool-activity ledger separately from
-- the model-authored summary. Re-compaction can merge the structured prior
-- ledger with newly shadowed calls without relying on Markdown parsing.

ALTER TABLE `context_compactions`
    ADD COLUMN `tool_activity_json` MEDIUMTEXT NULL
        COMMENT 'Structured merged tool-activity ledger (JSON)' AFTER `tool_activity_count`;
