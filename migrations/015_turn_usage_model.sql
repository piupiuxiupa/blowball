-- 015_turn_usage_model.sql
-- Per-turn model recording (per-request-model-selection capability; see
-- openspec per-request-model). A chat request may now select the turn's
-- model from the openai.models catalog; cost accounting needs to know which
-- model a historical turn actually ran on (per-model SUM() aggregation,
-- price-differentiated billing) — usage_json alone cannot answer it because
-- the model is not part of the usage object.
--
-- This migration adds turn_usage.model: the model name the turn resolved to
-- (the request-selected catalog entry when the request carried model /
-- reasoning_effort parameters, else the deployment default: the default
-- catalog entry, or the Confucius agent's configured model on the implicit
-- single-model catalog). The writer stores NULL for an empty value
-- (NULLIF), so rows written before this migration (NULL) and rows from
-- deployments that never resolve a model name read identically.
--
-- Index evaluation: deliberately NO index. The column exists for
-- operator-side analytics (GROUP BY model over a session or time range);
-- turn_usage is low-volume (one row per turn) and every read path already
-- scopes by session_id through the existing FK index, so a model index would
-- add write cost with no read benefit.
--
-- Existing databases must apply this migration manually (the docker-compose
-- initdb mount only runs on first volume init), mirroring migrations
-- 012/013/014.

ALTER TABLE `turn_usage`
    ADD COLUMN `model` VARCHAR(64) NULL
        COMMENT 'Model name this turn resolved to (request-selected catalog entry, else the deployment default); NULL on pre-migration rows' AFTER `total_tokens`;
