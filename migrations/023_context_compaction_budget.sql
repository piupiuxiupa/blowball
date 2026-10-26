-- 023_context_compaction_budget.sql
-- Budgeted context compaction (harden-context-compaction-budget).
--
-- The retained tail is now planned against a request input budget, not a fixed
-- message count. Large tool results in that tail are replaced by deterministic
-- summaries. Persist those summaries and the pre/post budget observations with
-- each new compaction record so later stitching is stable and failures are
-- diagnosable.
--
-- Every new column is nullable or defaulted. Existing records remain valid and
-- continue to use the legacy five-message stitching behavior until a new
-- compaction is written.

ALTER TABLE `context_compactions`
    ADD COLUMN `retained_results_json` MEDIUMTEXT NULL
        COMMENT 'Deterministic retained-tail tool-result summaries keyed by tool_call_id (JSON)' AFTER `summary_completion_tokens`,
    ADD COLUMN `measurement_basis` VARCHAR(24) NULL
        COMMENT 'provider_observed | estimated' AFTER `retained_results_json`,
    ADD COLUMN `pre_compaction_tokens` INT NULL
        COMMENT 'Estimated next-request input size before compaction' AFTER `measurement_basis`,
    ADD COLUMN `input_budget_tokens` INT NULL
        COMMENT 'Effective input budget at compaction time' AFTER `pre_compaction_tokens`,
    ADD COLUMN `post_compaction_tokens` INT NULL
        COMMENT 'Validated estimated next-request input size after compaction' AFTER `input_budget_tokens`,
    ADD COLUMN `retained_tail_messages` INT NULL
        COMMENT 'Agent messages retained verbatim or as deterministic summaries' AFTER `post_compaction_tokens`,
    ADD COLUMN `summary_strategy` VARCHAR(32) NULL
        COMMENT 'one_shot | chunked | fallback' AFTER `retained_tail_messages`,
    ADD COLUMN `tool_summary_count` INT NULL
        COMMENT 'Retained tool results replaced with deterministic summaries' AFTER `summary_strategy`,
    ADD COLUMN `tool_activity_count` INT NULL
        COMMENT 'Tool calls represented in the checkpoint activity ledger' AFTER `tool_summary_count`;
