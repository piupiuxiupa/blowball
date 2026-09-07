package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
)

const insertSubAgentInstanceSQL = `
INSERT INTO subagent_instances
    (session_id, agent_instance_id, parent_instance_id, depth, name, system_prompt, tools_json, latest_run_id)
VALUES
    (:session_id, :agent_instance_id, :parent_instance_id, :depth, :name, :system_prompt, :tools_json, :latest_run_id)
`

const updateSubAgentInstanceSQL = `
UPDATE subagent_instances
SET parent_instance_id = :parent_instance_id,
    depth = :depth,
    name = :name,
    system_prompt = :system_prompt,
    tools_json = :tools_json
WHERE session_id = :session_id AND agent_instance_id = :agent_instance_id
`

const advanceSubAgentLatestSQL = `
UPDATE subagent_instances
SET latest_run_id = ?
WHERE session_id = ? AND agent_instance_id = ?
`

const upsertSubAgentRunSQL = `
INSERT INTO subagent_runs
    (session_id, agent_instance_id, run_id, previous_run_id, run_no,
     parent_instance_id, depth, name, tools_json, status, snapshot_kind,
     resume_eligible, base_message_count, message_count, context_message_count,
     context_bytes, messages_json, started_at, finished_at)
VALUES
    (:session_id, :agent_instance_id, :run_id, :previous_run_id, :run_no,
     :parent_instance_id, :depth, :name, :tools_json, :status, :snapshot_kind,
     :resume_eligible, :base_message_count, :message_count, :context_message_count,
     :context_bytes, :messages_json, :started_at, :finished_at)
ON DUPLICATE KEY UPDATE
    agent_instance_id = VALUES(agent_instance_id),
    previous_run_id = VALUES(previous_run_id),
    run_no = VALUES(run_no),
    parent_instance_id = VALUES(parent_instance_id),
    depth = VALUES(depth),
    name = VALUES(name),
    tools_json = VALUES(tools_json),
    status = VALUES(status),
    snapshot_kind = VALUES(snapshot_kind),
    resume_eligible = VALUES(resume_eligible),
    base_message_count = VALUES(base_message_count),
    message_count = VALUES(message_count),
    context_message_count = VALUES(context_message_count),
    context_bytes = VALUES(context_bytes),
    messages_json = VALUES(messages_json),
    started_at = VALUES(started_at),
    finished_at = VALUES(finished_at)
`

const getSubAgentInstanceSQL = `
SELECT id, session_id, agent_instance_id,
       COALESCE(parent_instance_id, '') AS parent_instance_id,
       depth, name, system_prompt,
       COALESCE(tools_json, JSON_ARRAY()) AS tools_json,
       COALESCE(latest_run_id, '') AS latest_run_id,
       create_time, update_time
FROM subagent_instances
WHERE session_id = ? AND agent_instance_id = ?
FOR UPDATE
`

const listSubAgentRunsSQL = `
SELECT id, session_id, agent_instance_id, run_id,
       COALESCE(previous_run_id, '') AS previous_run_id,
       run_no, COALESCE(parent_instance_id, '') AS parent_instance_id,
       depth, name, COALESCE(tools_json, JSON_ARRAY()) AS tools_json,
       status, snapshot_kind, resume_eligible, base_message_count,
       message_count, context_message_count, context_bytes, messages_json,
       started_at, finished_at, create_time, update_time
FROM subagent_runs
WHERE session_id = ? AND agent_instance_id = ?
ORDER BY run_no ASC, id ASC
`

const getSubAgentRunSQL = `
SELECT id, session_id, agent_instance_id, run_id,
       COALESCE(previous_run_id, '') AS previous_run_id,
       run_no, COALESCE(parent_instance_id, '') AS parent_instance_id,
       depth, name, COALESCE(tools_json, JSON_ARRAY()) AS tools_json,
       status, snapshot_kind, resume_eligible, base_message_count,
       message_count, context_message_count, context_bytes, messages_json,
       started_at, finished_at, create_time, update_time
FROM subagent_runs
WHERE session_id = ? AND agent_instance_id = ? AND run_id = ?
`

const getSubAgentRunIdentitySQL = `
SELECT agent_instance_id, run_no FROM subagent_runs
WHERE session_id = ? AND run_id = ?
FOR UPDATE
`

// GetSubAgentInstance returns stable metadata for the exact session-scoped
// instance. ok=false covers both missing and cross-session targets.
func (s *Store) GetSubAgentInstance(ctx context.Context, sessionID, instanceID string) (model.SubAgentInstance, bool, error) {
	logQuery(ctx, "subagent_instance.get", getSubAgentInstanceSQL, sessionID, instanceID)
	return scanSubAgentInstance(s.db, ctx, sessionID, instanceID)
}

// ListSubAgentRuns returns all terminal run rows for one instance in execution
// order. An empty non-nil slice means the instance exists but has no rows (or
// is missing; callers that need to distinguish should load the instance).
func (s *Store) ListSubAgentRuns(ctx context.Context, sessionID, instanceID string) ([]model.SubAgentRun, error) {
	logQuery(ctx, "subagent_run.list", listSubAgentRunsSQL, sessionID, instanceID)
	var runs []model.SubAgentRun
	if err := s.db.SelectContext(ctx, &runs, listSubAgentRunsSQL, sessionID, instanceID); err != nil {
		return nil, fmt.Errorf("list sub-agent runs: %w", err)
	}
	if runs == nil {
		runs = make([]model.SubAgentRun, 0)
	}
	return runs, nil
}

// GetSubAgentRun returns one terminal delta scoped by session, instance and
// run identity.
func (s *Store) GetSubAgentRun(ctx context.Context, sessionID, instanceID, runID string) (model.SubAgentRun, bool, error) {
	logQuery(ctx, "subagent_run.get", getSubAgentRunSQL, sessionID, instanceID, runID)
	var run model.SubAgentRun
	if err := s.db.GetContext(ctx, &run, getSubAgentRunSQL, sessionID, instanceID, runID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.SubAgentRun{}, false, nil
		}
		return model.SubAgentRun{}, false, fmt.Errorf("get sub-agent run: %w", err)
	}
	return run, true, nil
}

// GetSubAgentHistory returns the stable instance metadata and its full ordered
// run chain. It intentionally does not decode MessagesJSON: the agent package
// owns the chat-message type and validates delta semantics before resume.
func (s *Store) GetSubAgentHistory(ctx context.Context, sessionID, instanceID string) (model.SubAgentHistory, bool, error) {
	instance, ok, err := s.GetSubAgentInstance(ctx, sessionID, instanceID)
	if err != nil || !ok {
		return model.SubAgentHistory{}, false, err
	}
	runs, err := s.ListSubAgentRuns(ctx, sessionID, instanceID)
	if err != nil {
		return model.SubAgentHistory{}, false, err
	}
	return model.SubAgentHistory{Instance: instance, Runs: runs}, true, nil
}

// AppendSubAgentRun transactionally upserts one terminal run and advances the
// instance's latest-run pointer. The run's PreviousRunID is the predecessor
// observed when dispatch began; if another writer has advanced the chain, this
// append rolls back and returns ErrSubAgentChainConflict. A retry reuses the
// same RunID and may overwrite that row when it is still the chain head.
func (s *Store) AppendSubAgentRun(ctx context.Context, instance model.SubAgentInstance, run model.SubAgentRun) error {
	if instance.SessionID == "" || instance.AgentInstanceID == "" || run.RunID == "" {
		return fmt.Errorf("append sub-agent run: session, instance and run identities are required")
	}
	if instance.SessionID != run.SessionID || instance.AgentInstanceID != run.AgentInstanceID {
		return fmt.Errorf("append sub-agent run: instance and run identities differ")
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append sub-agent run: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, ok, err := scanSubAgentInstance(tx, ctx, instance.SessionID, instance.AgentInstanceID)
	if err != nil {
		return err
	}
	if !ok {
		if run.PreviousRunID != "" {
			return model.ErrSubAgentChainConflict
		}
		if run.RunNo != 1 {
			return fmt.Errorf("append sub-agent run: initial run number must be 1, got %d", run.RunNo)
		}
		params := subAgentInstanceParams(instance)
		params["latest_run_id"] = nil
		logQuery(ctx, "subagent_instance.insert", insertSubAgentInstanceSQL)
		if _, err := tx.NamedExecContext(ctx, insertSubAgentInstanceSQL, params); err != nil {
			return fmt.Errorf("append sub-agent run: insert instance: %w", err)
		}
	} else {
		if current.LatestRunID != run.RunID && current.LatestRunID != run.PreviousRunID {
			return model.ErrSubAgentChainConflict
		}
		if current.LatestRunID == run.PreviousRunID && run.PreviousRunID != "" {
			var predecessor struct {
				AgentInstanceID string `db:"agent_instance_id"`
				RunNo           int    `db:"run_no"`
			}
			err = tx.GetContext(ctx, &predecessor, getSubAgentRunIdentitySQL, run.SessionID, run.PreviousRunID)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && predecessor.AgentInstanceID != run.AgentInstanceID) {
				return model.ErrSubAgentChainConflict
			}
			if err != nil {
				return fmt.Errorf("append sub-agent run: lock predecessor: %w", err)
			}
			if run.RunNo != predecessor.RunNo+1 {
				return fmt.Errorf("append sub-agent run: run number must follow predecessor: got %d, want %d", run.RunNo, predecessor.RunNo+1)
			}
		}
		params := subAgentInstanceParams(instance)
		logQuery(ctx, "subagent_instance.update", updateSubAgentInstanceSQL)
		if _, err := tx.NamedExecContext(ctx, updateSubAgentInstanceSQL, params); err != nil {
			return fmt.Errorf("append sub-agent run: update instance: %w", err)
		}
	}

	var identity struct {
		AgentInstanceID string `db:"agent_instance_id"`
		RunNo           int    `db:"run_no"`
	}
	err = tx.GetContext(ctx, &identity, getSubAgentRunIdentitySQL, run.SessionID, run.RunID)
	switch {
	case err == nil:
		if identity.AgentInstanceID != run.AgentInstanceID || identity.RunNo != run.RunNo {
			return fmt.Errorf("append sub-agent run: run identity collision on %q", run.RunID)
		}
	case errors.Is(err, sql.ErrNoRows):
		// A new run is inserted below.
	default:
		return fmt.Errorf("append sub-agent run: lock existing run: %w", err)
	}

	logQuery(ctx, "subagent_run.append", upsertSubAgentRunSQL)
	if _, err := tx.NamedExecContext(ctx, upsertSubAgentRunSQL, subAgentRunParams(run)); err != nil {
		return fmt.Errorf("append sub-agent run: upsert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, advanceSubAgentLatestSQL, run.RunID, instance.SessionID, instance.AgentInstanceID); err != nil {
		return fmt.Errorf("append sub-agent run: advance latest: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("append sub-agent run: commit: %w", err)
	}
	return nil
}

func scanSubAgentInstance(q sqlx.QueryerContext, ctx context.Context, sessionID, instanceID string) (model.SubAgentInstance, bool, error) {
	var instance model.SubAgentInstance
	if err := sqlx.GetContext(ctx, q, &instance, getSubAgentInstanceSQL, sessionID, instanceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.SubAgentInstance{}, false, nil
		}
		return model.SubAgentInstance{}, false, fmt.Errorf("get sub-agent instance: %w", err)
	}
	return instance, true, nil
}

func subAgentInstanceParams(instance model.SubAgentInstance) map[string]any {
	return map[string]any{
		"session_id":         instance.SessionID,
		"agent_instance_id":  instance.AgentInstanceID,
		"parent_instance_id": nilIfEmpty(instance.ParentInstanceID),
		"depth":              instance.Depth,
		"name":               instance.Name,
		"system_prompt":      instance.SystemPrompt,
		"tools_json":         nilIfEmptyJSON(instance.ToolsJSON),
		"latest_run_id":      nilIfEmpty(instance.LatestRunID),
	}
}

func subAgentRunParams(run model.SubAgentRun) map[string]any {
	return map[string]any{
		"session_id":            run.SessionID,
		"agent_instance_id":     run.AgentInstanceID,
		"run_id":                run.RunID,
		"previous_run_id":       nilIfEmpty(run.PreviousRunID),
		"run_no":                run.RunNo,
		"parent_instance_id":    nilIfEmpty(run.ParentInstanceID),
		"depth":                 run.Depth,
		"name":                  run.Name,
		"tools_json":            nilIfEmptyJSON(run.ToolsJSON),
		"status":                run.Status,
		"snapshot_kind":         run.SnapshotKind,
		"resume_eligible":       run.ResumeEligible,
		"base_message_count":    run.BaseMessageCount,
		"message_count":         run.MessageCount,
		"context_message_count": run.ContextMessageCount,
		"context_bytes":         run.ContextBytes,
		"messages_json":         nilIfEmptyJSON(run.MessagesJSON),
		"started_at":            run.StartedAt,
		"finished_at":           run.FinishedAt,
	}
}

func nilIfEmptyJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
