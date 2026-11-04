package model

import (
	"encoding/json"
	"time"

	"github.com/lush/blowball/internal/schedule"
)

const (
	ScheduledSessionNew      = "new"
	ScheduledSessionExisting = "existing"
)

const (
	ScheduledRunPending     = "pending"
	ScheduledRunPreparing   = "preparing"
	ScheduledRunLaunching   = "launching"
	ScheduledRunRunning     = "running"
	ScheduledRunSucceeded   = "succeeded"
	ScheduledRunError       = "error"
	ScheduledRunSkipped     = "skipped"
	ScheduledRunCancelled   = "cancelled"
	ScheduledRunInterrupted = "interrupted"
)

const (
	ScheduledReasonTaskOverlap        = "TASK_OVERLAP"
	ScheduledReasonSessionBusy        = "SESSION_BUSY"
	ScheduledReasonUserUnavailable    = "USER_UNAVAILABLE"
	ScheduledReasonSessionUnavailable = "SESSION_UNAVAILABLE"
	ScheduledReasonTaskTimeout        = "TASK_TIMEOUT"
	ScheduledReasonTaskPaused         = "TASK_PAUSED"
	ScheduledReasonTaskDeleted        = "TASK_DELETED"
	ScheduledReasonAdmission          = "ADMISSION_UNAVAILABLE"
	ScheduledReasonStartRejected      = "START_REJECTED"
	ScheduledReasonResultUnknown      = "RESULT_UNKNOWN"
)

// ScheduledRunTerminal reports whether no further execution or recovery is
// allowed for the occurrence.
func ScheduledRunTerminal(status string) bool {
	switch status {
	case ScheduledRunSucceeded, ScheduledRunError, ScheduledRunSkipped, ScheduledRunCancelled, ScheduledRunInterrupted:
		return true
	default:
		return false
	}
}

// ScheduledSessionPolicy selects a fresh session per occurrence or one
// explicitly targeted existing session.
type ScheduledSessionPolicy struct {
	Mode      string `json:"mode"`
	SessionID string `json:"session_id,omitempty"`
}

// ScheduledTask is both the storage row and the owner-facing task DTO. Secret
// credentials deliberately have no fields here.
type ScheduledTask struct {
	TaskID          string                 `db:"task_id" json:"task_id"`
	UserID          string                 `db:"user_id" json:"-"`
	Name            string                 `db:"name" json:"name"`
	Prompt          string                 `db:"prompt" json:"prompt"`
	ScheduleType    string                 `db:"schedule_type" json:"-"`
	RunAt           *time.Time             `db:"run_at" json:"-"`
	CronExpression  string                 `db:"cron_expression" json:"-"`
	Timezone        string                 `db:"timezone" json:"-"`
	SessionMode     string                 `db:"session_mode" json:"-"`
	SessionID       *string                `db:"session_id" json:"-"`
	Model           string                 `db:"model" json:"model,omitempty"`
	ReasoningEffort string                 `db:"reasoning_effort" json:"reasoning_effort,omitempty"`
	Enabled         bool                   `db:"enabled" json:"enabled"`
	NextRunAt       *time.Time             `db:"next_run_at" json:"next_run_at,omitempty"`
	Revision        int64                  `db:"revision" json:"revision"`
	PausedReason    string                 `db:"paused_reason" json:"paused_reason,omitempty"`
	DeletedAt       *time.Time             `db:"deleted_at" json:"-"`
	UpdateTime      time.Time              `db:"update_time" json:"update_time"`
	CreateTime      time.Time              `db:"create_time" json:"create_time"`
	Schedule        schedule.Rule          `db:"-" json:"schedule"`
	Session         ScheduledSessionPolicy `db:"-" json:"session"`
}

// ApplyDefaults normalizes optional non-secret task fields.
func (t *ScheduledTask) ApplyDefaults() {
	if t.SessionMode == "" {
		t.SessionMode = ScheduledSessionNew
	}
	if t.Revision <= 0 {
		t.Revision = 1
	}
	t.HydrateDerived()
}

// SetSchedule validates no timing; it only normalizes the storage projection
// after the service has validated the rule.
func (t *ScheduledTask) SetSchedule(rule schedule.Rule) {
	t.Schedule = rule
	t.ScheduleType = rule.Type
	t.CronExpression = rule.Expression
	t.Timezone = rule.Timezone
	t.RunAt = nil
	if rule.Type == schedule.TypeAt {
		if runAt, err := schedule.Next(rule, time.Time{}); err == nil {
			runAt = runAt.UTC()
			t.RunAt = &runAt
		}
	}
}

// Rule rebuilds the structured schedule from its storage projection.
func (t *ScheduledTask) Rule() schedule.Rule {
	rule := schedule.Rule{Type: t.ScheduleType, Expression: t.CronExpression, Timezone: t.Timezone}
	if t.RunAt != nil {
		rule.RunAt = t.RunAt.UTC().Format(time.RFC3339Nano)
	}
	return rule
}

// HydrateDerived populates the API-only schedule and session projections.
func (t *ScheduledTask) HydrateDerived() {
	t.Schedule = t.Rule()
	t.Session = ScheduledSessionPolicy{Mode: t.SessionMode}
	if t.SessionID != nil {
		t.Session.SessionID = *t.SessionID
	}
}

// ScheduledTaskSnapshot is the immutable non-secret configuration captured for
// one materialized occurrence. Empty model/effort mean the deployment default
// at execution resolution time.
type ScheduledTaskSnapshot struct {
	TaskID          string                 `json:"task_id"`
	UserID          string                 `json:"user_id"`
	Revision        int64                  `json:"revision"`
	Name            string                 `json:"name"`
	Prompt          string                 `json:"prompt"`
	Schedule        schedule.Rule          `json:"schedule"`
	Session         ScheduledSessionPolicy `json:"session"`
	Model           string                 `json:"model,omitempty"`
	ReasoningEffort string                 `json:"reasoning_effort,omitempty"`
}

func (t *ScheduledTask) Snapshot() ScheduledTaskSnapshot {
	mode := t.SessionMode
	if mode == "" {
		mode = ScheduledSessionNew
	}
	session := ScheduledSessionPolicy{Mode: mode}
	if t.SessionID != nil {
		session.SessionID = *t.SessionID
	}
	return ScheduledTaskSnapshot{
		TaskID: t.TaskID, UserID: t.UserID, Revision: t.Revision,
		Name: t.Name, Prompt: t.Prompt, Schedule: t.Rule(),
		Session: session,
		Model:   t.Model, ReasoningEffort: t.ReasoningEffort,
	}
}

// ScheduledTaskRun is one durable occurrence/execution history row.
type ScheduledTaskRun struct {
	ID            int64      `db:"id" json:"-"`
	OccurrenceID  string     `db:"occurrence_id" json:"occurrence_id"`
	TaskID        string     `db:"task_id" json:"task_id"`
	UserID        string     `db:"user_id" json:"-"`
	Revision      int64      `db:"revision" json:"revision"`
	SnapshotJSON  string     `db:"snapshot_json" json:"-"`
	ScheduledFor  time.Time  `db:"scheduled_for" json:"scheduled_for"`
	RunID         string     `db:"run_id" json:"run_id"`
	SessionID     string     `db:"session_id" json:"session_id"`
	SessionMode   string     `db:"session_mode" json:"session_mode"`
	Status        string     `db:"status" json:"status"`
	Reason        string     `db:"reason" json:"reason,omitempty"`
	ClaimToken    string     `db:"claim_token" json:"-"`
	LeaseUntil    *time.Time `db:"lease_until" json:"-"`
	AttemptCount  int        `db:"attempt_count" json:"attempt_count"`
	NextAttemptAt *time.Time `db:"next_attempt_at" json:"next_attempt_at,omitempty"`
	StartedAt     *time.Time `db:"started_at" json:"started_at,omitempty"`
	FinishedAt    *time.Time `db:"finished_at" json:"finished_at,omitempty"`
	LastError     string     `db:"last_error" json:"error,omitempty"`
	UpdateTime    time.Time  `db:"update_time" json:"update_time"`
	CreateTime    time.Time  `db:"create_time" json:"create_time"`
}

func (r *ScheduledTaskRun) Snapshot() (ScheduledTaskSnapshot, error) {
	var snapshot ScheduledTaskSnapshot
	err := json.Unmarshal([]byte(r.SnapshotJSON), &snapshot)
	return snapshot, err
}
