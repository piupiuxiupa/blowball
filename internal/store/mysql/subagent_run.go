package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
)

const upsertSubAgentRunSQL = `
INSERT INTO subagent_runs
    (session_id, agent_instance_id, parent_instance_id, depth, name, tools_json, status, resume_eligible, messages_json)
VALUES
    (:session_id, :agent_instance_id, :parent_instance_id, :depth, :name, :tools_json, :status, :resume_eligible, :messages_json)
ON DUPLICATE KEY UPDATE
    parent_instance_id = VALUES(parent_instance_id),
    depth = VALUES(depth),
    name = VALUES(name),
    tools_json = VALUES(tools_json),
    status = VALUES(status),
    resume_eligible = VALUES(resume_eligible),
    messages_json = VALUES(messages_json)
`

const getSubAgentRunSQL = `
SELECT
    id, session_id, agent_instance_id,
    COALESCE(parent_instance_id, '') AS parent_instance_id,
    depth, name,
    COALESCE(tools_json, JSON_ARRAY()) AS tools_json,
    status, resume_eligible, messages_json, create_time, update_time
FROM subagent_runs
WHERE session_id = ? AND agent_instance_id = ?
`

// UpsertSubAgentRun atomically replaces the authoritative snapshot for one
// (session, instance) pair. Callers invoke it only after a run terminates, so
// the last terminal state wins across initial dispatches and resumes.
func (s *Store) UpsertSubAgentRun(ctx context.Context, run model.SubAgentRun) error {
	params := map[string]any{
		"session_id":         run.SessionID,
		"agent_instance_id":  run.AgentInstanceID,
		"parent_instance_id": nilIfEmpty(run.ParentInstanceID),
		"depth":              run.Depth,
		"name":               run.Name,
		"tools_json":         nilIfEmptyJSON(run.ToolsJSON),
		"status":             run.Status,
		"resume_eligible":    run.ResumeEligible,
		"messages_json":      run.MessagesJSON,
	}
	logQuery(ctx, "subagent_run.upsert", upsertSubAgentRunSQL)
	if _, err := sqlx.NamedExecContext(ctx, s.db, upsertSubAgentRunSQL, params); err != nil {
		return fmt.Errorf("upsert subagent run: %w", err)
	}
	return nil
}

// GetSubAgentRun returns the authoritative snapshot for the exact
// (sessionID, instanceID) pair. ok=false covers both a missing instance and an
// instance belonging to another session; callers surface both as invalid
// resume targets without leaking cross-session existence.
func (s *Store) GetSubAgentRun(ctx context.Context, sessionID, instanceID string) (model.SubAgentRun, bool, error) {
	logQuery(ctx, "subagent_run.get", getSubAgentRunSQL, sessionID, instanceID)
	var run model.SubAgentRun
	if err := sqlx.GetContext(ctx, s.db, &run, getSubAgentRunSQL, sessionID, instanceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.SubAgentRun{}, false, nil
		}
		return model.SubAgentRun{}, false, fmt.Errorf("get subagent run: %w", err)
	}
	return run, true, nil
}

func nilIfEmptyJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
