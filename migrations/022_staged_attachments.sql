-- Staged (session-independent) attachment upload (add-staged-attachment-upload).
--
-- attachments.session_id becomes nullable: NULL marks a staged row — uploaded
-- via POST /api/v1/attachments before any session exists and claimed into a
-- session at send time. The existing foreign key is unaffected (NULL bypasses
-- FK enforcement, and the claim UPDATE writes a session id that SendMessage
-- has already resolved). ix_attachments_session_created backs the orphan
-- sweeper scan (session_id IS NULL AND create_time < cutoff).
--
-- attachments_deleted keeps its verbatim-mirror convention; staged rows never
-- reach it (claimed or TTL-swept, never session-archived), but the column is
-- relaxed alongside so the mirror schema never drifts from the live table.

ALTER TABLE `attachments`
    MODIFY `session_id` CHAR(36) NULL COMMENT 'Owning session; NULL = staged (uploaded, not yet claimed by a session)',
    ADD KEY `ix_attachments_session_created` (`session_id`, `create_time`);

ALTER TABLE `attachments_deleted`
    MODIFY `session_id` CHAR(36) NULL COMMENT 'Original attachments.session_id (NULL when the row was staged)';
