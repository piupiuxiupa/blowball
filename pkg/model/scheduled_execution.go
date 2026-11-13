package model

import "time"

// Runtime execution receipt states (main-service owned). The Scheduler's
// occurrence status is a projection; these states are the authoritative
// execution facts.
const (
	ScheduledReceiptAccepted    = "accepted"
	ScheduledReceiptPreparing   = "preparing"
	ScheduledReceiptStartIntent = "start_intent"
	ScheduledReceiptRunning     = "running"
	ScheduledReceiptSucceeded   = "succeeded"
	ScheduledReceiptError       = "error"
	ScheduledReceiptCancelled   = "cancelled"
	ScheduledReceiptInterrupted = "interrupted"
	ScheduledReceiptSkipped     = "skipped"
	ScheduledReceiptUncertain   = "uncertain"
)

// Stop-proof kinds. A proof is written only when the owning Runtime positively
// confirmed the agent/managed commands terminated and fenced their generation.
const (
	ScheduledStopProofNotStarted = "not_started" // terminal before any side effect
	ScheduledStopProofTerminal   = "terminal"    // normal turn completion
	ScheduledStopProofReconciled = "reconciled"  // controlled operator reconciliation
)

// ScheduledExecutionReceipt is one occurrence's authoritative execution
// record. It carries no credential of any kind.
type ScheduledExecutionReceipt struct {
	OccurrenceID      string     `db:"occurrence_id"`
	TaskID            string     `db:"task_id"`
	UserID            string     `db:"user_id"`
	RunID             string     `db:"run_id"`
	SessionID         string     `db:"session_id"`
	AttemptCount      int        `db:"attempt_count"`
	RuntimeAttempts   int        `db:"runtime_attempts"`
	ConfigFingerprint string     `db:"config_fingerprint"`
	ReceiptGeneration uint64     `db:"receipt_generation"`
	Status            string     `db:"status"`
	Reason            string     `db:"reason"`
	LastError         string     `db:"last_error"`
	WorkerGeneration  string     `db:"worker_generation"`
	LeaseUntil        *time.Time `db:"lease_until"`
	AcceptedAt        time.Time  `db:"accepted_at"`
	StartIntentAt     *time.Time `db:"start_intent_at"`
	StartedAt         *time.Time `db:"started_at"`
	FinishedAt        *time.Time `db:"finished_at"`
	GrantGeneration   uint64     `db:"grant_generation"`
	GrantDeadline     *time.Time `db:"grant_deadline"`
	StopProofPresent  bool       `db:"stop_proof_present"`
	StopProofGen      uint64     `db:"stop_proof_generation"`
	StopProofKind     string     `db:"stop_proof_kind"`
	StopProofAt       *time.Time `db:"stop_proof_at"`
	CreateTime        time.Time  `db:"create_time"`
	UpdateTime        time.Time  `db:"update_time"`
}

// MayBeRunning reports whether the execution might still have side effects.
// It is the task-hold/delete-guard predicate.
func (r ScheduledExecutionReceipt) MayBeRunning() bool {
	switch r.Status {
	case ScheduledReceiptAccepted, ScheduledReceiptPreparing:
		return false // no start_intent crossed yet
	case ScheduledReceiptStartIntent, ScheduledReceiptRunning, ScheduledReceiptUncertain:
		return !r.StopProofPresent
	default:
		return false
	}
}

// Terminal reports whether the receipt holds a terminal fact.
func (r ScheduledExecutionReceipt) Terminal() bool {
	switch r.Status {
	case ScheduledReceiptSucceeded, ScheduledReceiptError, ScheduledReceiptCancelled,
		ScheduledReceiptInterrupted, ScheduledReceiptSkipped:
		return true
	default:
		return false
	}
}

// Active reports whether the receipt counts toward managed capacity.
func (r ScheduledExecutionReceipt) Active() bool {
	switch r.Status {
	case ScheduledReceiptPreparing, ScheduledReceiptStartIntent, ScheduledReceiptRunning, ScheduledReceiptUncertain:
		return true
	default:
		return false
	}
}

// ScheduledStartGrant is the neutral form of the Scheduler's unique start
// authorization.
type ScheduledStartGrant struct {
	OccurrenceID string
	Generation   uint64
	RunID        string
	SessionID    string
	StartedAt    time.Time
	Deadline     time.Time
}
