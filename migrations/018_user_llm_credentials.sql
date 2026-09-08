-- Per-user LLM API tokens (user-llm-token capability).
--
-- One row per user: the plaintext gateway token that replaces the global
-- openai.api_key for every LLM call attributable to that user. The shared
-- openai.base_url, model catalog and all model metadata stay global; this
-- table only swaps the credential axis. Tokens are never returned by the
-- management API (only a masked preview) and never logged.

CREATE TABLE IF NOT EXISTS `user_llm_credentials` (
    `user_id`     VARCHAR(64)  NOT NULL COMMENT 'users.user_id (no FK; PK isolation suffices)',
    `api_key`     VARCHAR(512) NOT NULL COMMENT 'Plaintext gateway token; replaces openai.api_key for this user',
    `update_time` TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `create_time` TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Per-user LLM gateway credentials';
