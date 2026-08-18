-- 012_client_msg_id.sql
-- Idempotency key for the Redis-first write-behind message persistence
-- (message-write-behind capability). Message batches are dual-written into
-- Redis (read cache msgs:{sid} + ingest queue msgs:buffer) and inserted into
-- MySQL asynchronously by the background flusher, so a single logical message
-- can be inserted more than once (crash between INSERT and ack, retry after a
-- failed insert, fallback direct-write racing the flusher). The UNIQUE index
-- below plus INSERT IGNORE in the store layer collapse every redelivery to
-- exactly one row.
--
-- Legacy rows keep NULL — MySQL UNIQUE indexes allow multiple NULLs, so
-- pre-existing data is unaffected. Existing databases must apply this
-- migration manually (the docker-compose initdb mount only runs on first
-- volume init).

ALTER TABLE `messages`
    ADD COLUMN `client_msg_id` CHAR(36) NULL COMMENT 'Idempotency key minted at persistence time (UUID); NULL on legacy rows' AFTER `trace_id`,
    ADD UNIQUE KEY `uk_messages_client_msg_id` (`client_msg_id`);
