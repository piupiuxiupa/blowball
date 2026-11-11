package mysql

import (
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
)

// Tables is the fixed, server-side map from a session source to its physical
// table group. Table names are compile-time constants selected by an enum; no
// client input can ever reach an identifier here.
type Tables struct {
	Sessions           string
	Messages           string
	ToolCalls          string
	Titles             string
	TurnUsage          string
	ContextCompactions string
	SubAgentInstances  string
	SubAgentRuns       string
	Attachments        string

	// Archive mirrors used by DeleteSession.
	SessionsDeleted    string
	MessagesDeleted    string
	TitlesDeleted      string
	AttachmentsDeleted string

	// Scheduled marks the physically separate task-session group. It changes
	// the session archive shape (task linkage columns) and forbids the
	// interactive-only staged-attachment surface.
	Scheduled bool
}

var interactiveTables = Tables{
	Sessions:           "sessions",
	Messages:           "messages",
	ToolCalls:          "tool_calls",
	Titles:             "titles",
	TurnUsage:          "turn_usage",
	ContextCompactions: "context_compactions",
	SubAgentInstances:  "subagent_instances",
	SubAgentRuns:       "subagent_runs",
	Attachments:        "attachments",

	SessionsDeleted:    "sessions_deleted",
	MessagesDeleted:    "messages_deleted",
	TitlesDeleted:      "titles_deleted",
	AttachmentsDeleted: "attachments_deleted",
}

var scheduledTables = Tables{
	Sessions:           "scheduled_sessions",
	Messages:           "scheduled_messages",
	ToolCalls:          "scheduled_tool_calls",
	Titles:             "scheduled_titles",
	TurnUsage:          "scheduled_turn_usage",
	ContextCompactions: "scheduled_context_compactions",
	SubAgentInstances:  "scheduled_subagent_instances",
	SubAgentRuns:       "scheduled_subagent_runs",
	Attachments:        "scheduled_attachments",

	SessionsDeleted:    "scheduled_sessions_deleted",
	MessagesDeleted:    "scheduled_messages_deleted",
	TitlesDeleted:      "scheduled_titles_deleted",
	AttachmentsDeleted: "scheduled_attachments_deleted",

	Scheduled: true,
}

// TablesFor returns the table group for a source. An unknown/empty source is an
// error (fail closed) — the interactive default is applied by the request
// layer, never guessed by the store.
func TablesFor(source model.SessionSource) (Tables, error) {
	switch source {
	case model.SessionSourceInteractive, model.SessionSourceLegacyInteractive:
		return interactiveTables, nil
	case model.SessionSourceScheduled:
		return scheduledTables, nil
	default:
		return Tables{}, fmt.Errorf("mysql: unknown session source %q", source)
	}
}

// SessionStore is a source-bound view over the MySQL store: every method
// operates on exactly one table group for its whole lifetime. Services receive
// one of these instead of the raw *Store, so a persistence call can never
// silently fall back to the ordinary tables.
type SessionStore struct {
	db     *sqlx.DB
	tables Tables
	source model.SessionSource
}

// ForSource binds the store to a fixed source. The returned view implements the
// same persistence surfaces as *Store (session/message/title/usage/compaction/
// sub-agent), but always resolves identifiers through one table group.
func (s *Store) ForSource(source model.SessionSource) *SessionStore {
	tables, err := TablesFor(source)
	if err != nil {
		// Callers only pass enum constants; an unknown source is a programming
		// error, and panicking here is safer than silently using the wrong
		// conversation tables.
		panic(err)
	}
	return &SessionStore{db: s.db, tables: tables, source: source}
}

// Source returns the fixed source of this view.
func (s *SessionStore) Source() model.SessionSource { return s.source }

// scheduled reports whether this view uses the task-session table group.
func (s *SessionStore) scheduled() bool { return s.tables.Scheduled }
