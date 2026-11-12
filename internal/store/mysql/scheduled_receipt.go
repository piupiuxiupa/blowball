package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
)

// ErrScheduledReceiptConflict reports that an idempotent acceptance found a
// receipt with a different configuration fingerprint or execution identity.
// It is a permanent conflict, never a retryable one.
var ErrScheduledReceiptConflict = errors.New("scheduled execution receipt conflict")

const scheduledReceiptColumns = `occurrence_id, task_id, user_id, run_id, session_id, attempt_count,
       runtime_attempts,
       config_fingerprint, receipt_generation, status, reason, COALESCE(last_error, '') AS last_error,
       worker_generation, lease_until, accepted_at, start_intent_at, started_at, finished_at,
       grant_generation, grant_deadline, stop_proof_present, stop_proof_generation,
       stop_proof_kind, stop_proof_at, create_time, update_time`

const scheduledAdmissionLock = "blowball:scheduled:admission"
const scheduledAdmissionLockWait = 2

// AcceptScheduledExecution transactionally creates the occurrence's scheduled
// session and its execution receipt. Both identities are pre-allocated by the
// Scheduler, so a repeated submission must resolve to the SAME rows; a
// different fingerprint or execution identity is a permanent conflict.
func (s *Store) AcceptScheduledExecution(ctx context.Context, receipt model.ScheduledExecutionReceipt, session model.Session) (*model.ScheduledExecutionReceipt, bool, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("mysql.AcceptScheduledExecution: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1) Scheduled session (idempotent by session_id).
	insertSession := `INSERT INTO scheduled_sessions
    (session_id, user_id, task_id, occurrence_id, run_id, trace_id)
VALUES (?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE session_id = session_id`
	if _, err := tx.ExecContext(ctx, insertSession,
		session.SessionID, session.UserID, session.TaskID, session.OccurrenceID, session.RunID, session.TraceID); err != nil {
		return nil, false, fmt.Errorf("mysql.AcceptScheduledExecution: session: %w", err)
	}
	var storedSession model.Session
	if err := tx.GetContext(ctx, &storedSession, `SELECT session_id, user_id, task_id, occurrence_id, run_id, trace_id
FROM scheduled_sessions WHERE session_id = ? FOR UPDATE`, session.SessionID); err != nil {
		return nil, false, fmt.Errorf("mysql.AcceptScheduledExecution: read session: %w", err)
	}
	if storedSession.UserID != session.UserID || storedSession.TaskID != session.TaskID ||
		storedSession.OccurrenceID != session.OccurrenceID || storedSession.RunID != session.RunID {
		return nil, false, fmt.Errorf("%w: session identity mismatch for %s", ErrScheduledReceiptConflict, session.SessionID)
	}

	// 2) Receipt (idempotent by occurrence_id).
	var existingID string
	err = tx.GetContext(ctx, &existingID, `SELECT occurrence_id FROM scheduled_execution_receipts WHERE occurrence_id = ? FOR UPDATE`, receipt.OccurrenceID)
	already := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("mysql.AcceptScheduledExecution: probe receipt: %w", err)
	}
	if !already {
		insertReceipt := `INSERT INTO scheduled_execution_receipts
    (occurrence_id, task_id, user_id, run_id, session_id, attempt_count, config_fingerprint,
     receipt_generation, status, accepted_at, worker_generation)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')`
		if _, err := tx.ExecContext(ctx, insertReceipt,
			receipt.OccurrenceID, receipt.TaskID, receipt.UserID, receipt.RunID, receipt.SessionID,
			receipt.AttemptCount, receipt.ConfigFingerprint, receipt.ReceiptGeneration,
			model.ScheduledReceiptAccepted, receipt.AcceptedAt.UTC()); err != nil {
			return nil, false, fmt.Errorf("mysql.AcceptScheduledExecution: receipt: %w", err)
		}
	} else {
		// Retry control (attempt) may advance; the configuration identity may
		// not. The fingerprint comparison below enforces that.
		if _, err := tx.ExecContext(ctx, `UPDATE scheduled_execution_receipts
SET attempt_count = GREATEST(attempt_count, ?)
WHERE occurrence_id = ?`, receipt.AttemptCount, receipt.OccurrenceID); err != nil {
			return nil, false, fmt.Errorf("mysql.AcceptScheduledExecution: advance attempt: %w", err)
		}
	}

	var stored model.ScheduledExecutionReceipt
	if err := tx.GetContext(ctx, &stored, `SELECT `+scheduledReceiptColumns+` FROM scheduled_execution_receipts WHERE occurrence_id = ? FOR UPDATE`, receipt.OccurrenceID); err != nil {
		return nil, false, fmt.Errorf("mysql.AcceptScheduledExecution: read receipt: %w", err)
	}
	if stored.TaskID != receipt.TaskID || stored.RunID != receipt.RunID ||
		stored.SessionID != receipt.SessionID || stored.UserID != receipt.UserID ||
		stored.ConfigFingerprint != receipt.ConfigFingerprint {
		return nil, false, fmt.Errorf("%w: occurrence %s", ErrScheduledReceiptConflict, receipt.OccurrenceID)
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("mysql.AcceptScheduledExecution: commit: %w", err)
	}
	return &stored, already, nil
}

// GetScheduledExecution returns the authoritative receipt for an occurrence,
// or (nil, nil) when no acceptance exists.
func (s *Store) GetScheduledExecution(ctx context.Context, occurrenceID string) (*model.ScheduledExecutionReceipt, error) {
	var rec model.ScheduledExecutionReceipt
	err := s.db.GetContext(ctx, &rec, `SELECT `+scheduledReceiptColumns+` FROM scheduled_execution_receipts WHERE occurrence_id = ?`, occurrenceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// GetScheduledExecutionBySession returns the receipt bound to a scheduled
// session (the delete/liveness guard), or (nil, nil).
func (s *Store) GetScheduledExecutionBySession(ctx context.Context, sessionID string) (*model.ScheduledExecutionReceipt, error) {
	var rec model.ScheduledExecutionReceipt
	err := s.db.GetContext(ctx, &rec, `SELECT `+scheduledReceiptColumns+` FROM scheduled_execution_receipts WHERE session_id = ?`, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// ClaimableScheduledReceipt is the worker scan row.
func (s *Store) ClaimableScheduledReceipt(ctx context.Context, occurrenceID string) (*model.ScheduledExecutionReceipt, error) {
	return s.GetScheduledExecution(ctx, occurrenceID)
}

// ListClaimableScheduledReceipts returns receipts that a worker may claim: a
// fresh acceptance, or a preparation whose lease expired (its worker never
// crossed start_intent).
func (s *Store) ListClaimableScheduledReceipts(ctx context.Context, now time.Time, limit int) ([]model.ScheduledExecutionReceipt, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT ` + scheduledReceiptColumns + ` FROM scheduled_execution_receipts
WHERE status = 'accepted'
   OR (status = 'preparing' AND (lease_until IS NULL OR lease_until < ?))
ORDER BY accepted_at ASC
LIMIT ?`
	var out []model.ScheduledExecutionReceipt
	if err := s.db.SelectContext(ctx, &out, query, now.UTC(), limit); err != nil {
		return nil, err
	}
	return out, nil
}

// ListLiveScheduledReceipts returns executions that may still be running
// (start_intent or running). Reconciliation checks their owner heartbeat.
func (s *Store) ListLiveScheduledReceipts(ctx context.Context, limit int) ([]model.ScheduledExecutionReceipt, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT ` + scheduledReceiptColumns + ` FROM scheduled_execution_receipts
WHERE status IN ('start_intent','running')
ORDER BY accepted_at ASC
LIMIT ?`
	var out []model.ScheduledExecutionReceipt
	if err := s.db.SelectContext(ctx, &out, query, limit); err != nil {
		return nil, err
	}
	return out, nil
}

// ClaimScheduledReceipt CAS-claims a receipt for one worker generation. A
// reclaim of an expired preparing lease bumps the receipt generation, fencing
// the previous worker's start-intent/running writes. Capacity is enforced under
// a named lock so multiple Runtime replicas cannot exceed maxActive together.
func (s *Store) ClaimScheduledReceipt(ctx context.Context, occurrenceID, workerGeneration string, lease time.Duration, maxActive int, now time.Time) (*model.ScheduledExecutionReceipt, bool, error) {
	var claimed *model.ScheduledExecutionReceipt
	ok := false
	err := s.withNamedLock(ctx, scheduledAdmissionLock, scheduledAdmissionLockWait, func(conn *sqlx.Conn) error {
		tx, err := conn.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()

		var current model.ScheduledExecutionReceipt
		if err := tx.GetContext(ctx, &current, `SELECT `+scheduledReceiptColumns+` FROM scheduled_execution_receipts WHERE occurrence_id = ? FOR UPDATE`, occurrenceID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		claimable := current.Status == model.ScheduledReceiptAccepted ||
			(current.Status == model.ScheduledReceiptPreparing && (current.LeaseUntil == nil || current.LeaseUntil.Before(now.UTC())))
		if !claimable {
			return nil
		}
		if maxActive > 0 {
			var active int
			if err := tx.GetContext(ctx, &active, `SELECT COUNT(*) FROM scheduled_execution_receipts
WHERE status IN ('preparing','start_intent','running','uncertain')`); err != nil {
				return err
			}
			// Reclaiming an already-preparing execution does not increase the
			// managed capacity; only a fresh acceptance does.
			if active >= maxActive && current.Status != model.ScheduledReceiptPreparing {
				return nil
			}
		}
		generation := current.ReceiptGeneration
		if current.Status == model.ScheduledReceiptPreparing {
			generation++ // fence the previous preparation worker
		}
		res, err := tx.ExecContext(ctx, `UPDATE scheduled_execution_receipts
SET status = 'preparing', worker_generation = ?, lease_until = ?, receipt_generation = ?,
    reason = '', last_error = NULL, start_intent_at = NULL
WHERE occurrence_id = ? AND status IN ('accepted','preparing') AND receipt_generation = ?`,
			workerGeneration, now.UTC().Add(lease), generation, occurrenceID, current.ReceiptGeneration)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return nil
		}
		if err := tx.GetContext(ctx, &current, `SELECT `+scheduledReceiptColumns+` FROM scheduled_execution_receipts WHERE occurrence_id = ?`, occurrenceID); err != nil {
			return err
		}
		claimed = &current
		ok = true
		return tx.Commit()
	})
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return claimed, true, nil
}

// RenewScheduledReceiptLease re-arms the worker lease while preparation or
// execution continues.
func (s *Store) RenewScheduledReceiptLease(ctx context.Context, occurrenceID, workerGeneration string, lease time.Duration, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_execution_receipts SET lease_until = ?
WHERE occurrence_id = ? AND worker_generation = ? AND status IN ('preparing','start_intent','running')`,
		now.UTC().Add(lease), occurrenceID, workerGeneration)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MarkScheduledReceiptStartIntent persists the start intent BEFORE the
// Scheduler authorization call. It succeeds only for the current worker and
// generation.
func (s *Store) MarkScheduledReceiptStartIntent(ctx context.Context, occurrenceID string, generation uint64, workerGeneration string, lease time.Duration, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_execution_receipts
SET status = 'start_intent', start_intent_at = ?, lease_until = ?
WHERE occurrence_id = ? AND receipt_generation = ? AND worker_generation = ? AND status = 'preparing'`,
		now.UTC(), now.UTC().Add(lease), occurrenceID, generation, workerGeneration)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleaseScheduledReceiptForRetry handles a fail-closed pre-start failure
// (admission storage unavailable, session busy, shutdown before submission).
// It increments the bounded runtime preparation counter: below maxAttempts the
// receipt returns to `accepted` for the next worker cycle; at the cap it
// becomes a terminal error with a not_started stop proof. It is only valid
// before start_intent, where no side effect can have occurred.
func (s *Store) ReleaseScheduledReceiptForRetry(ctx context.Context, occurrenceID string, generation uint64, workerGeneration, reason, lastError string, maxAttempts int, now time.Time) (bool, error) {
	retry := false
	err := func() error {
		tx, err := s.db.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		var cur struct {
			RuntimeAttempts int    `db:"runtime_attempts"`
			Status          string `db:"status"`
		}
		if err := tx.GetContext(ctx, &cur, `SELECT runtime_attempts, status FROM scheduled_execution_receipts
WHERE occurrence_id = ? AND receipt_generation = ? AND worker_generation = ? FOR UPDATE`,
			occurrenceID, generation, workerGeneration); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if cur.Status != model.ScheduledReceiptAccepted && cur.Status != model.ScheduledReceiptPreparing {
			return nil
		}
		attempts := cur.RuntimeAttempts + 1
		if attempts < maxAttempts {
			if _, err := tx.ExecContext(ctx, `UPDATE scheduled_execution_receipts
SET status = 'accepted', runtime_attempts = ?, worker_generation = '', lease_until = NULL,
    reason = ?, last_error = NULLIF(?, '')
WHERE occurrence_id = ?`, attempts, reason, lastError, occurrenceID); err != nil {
				return err
			}
			retry = true
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE scheduled_execution_receipts
SET status = 'error', runtime_attempts = ?, worker_generation = '', lease_until = NULL,
    reason = ?, last_error = NULLIF(?, ''), finished_at = ?,
    stop_proof_present = 1, stop_proof_generation = ?, stop_proof_kind = ?, stop_proof_at = ?
WHERE occurrence_id = ?`,
				attempts, reason, lastError, now.UTC(), generation, model.ScheduledStopProofNotStarted, now.UTC(), occurrenceID); err != nil {
				return err
			}
		}
		return tx.Commit()
	}()
	if err != nil {
		return false, err
	}
	return retry, nil
}

// CancelScheduledReceiptBeforeStart terminalizes a receipt that has not
// crossed start_intent (no side effect can have occurred). It intentionally
// does not require a worker generation: an explicit cancel competes with the
// preparation worker, and the status change fences that worker's later CAS.
func (s *Store) CancelScheduledReceiptBeforeStart(ctx context.Context, occurrenceID string, generation uint64, reason, lastError string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_execution_receipts
SET status = 'cancelled', reason = ?, last_error = NULLIF(?, ''), finished_at = ?, lease_until = NULL,
    stop_proof_present = 1, stop_proof_generation = ?, stop_proof_kind = ?, stop_proof_at = ?
WHERE occurrence_id = ? AND receipt_generation = ? AND status IN ('accepted','preparing')`,
		reason, lastError, now.UTC(), generation, model.ScheduledStopProofNotStarted, now.UTC(), occurrenceID, generation)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleaseScheduledReceiptOnShutdown returns a pre-start receipt to `accepted`
// without consuming a preparation attempt. Process shutdown is not a
// preparation failure; the post-restart Runtime claims it again.
func (s *Store) ReleaseScheduledReceiptOnShutdown(ctx context.Context, occurrenceID string, generation uint64, workerGeneration string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_execution_receipts
SET status = 'accepted', worker_generation = '', lease_until = NULL
WHERE occurrence_id = ? AND receipt_generation = ? AND worker_generation = ? AND status IN ('accepted','preparing')`,
		occurrenceID, generation, workerGeneration)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FinishScheduledReceiptStartRejected terminalizes a receipt whose
// AuthorizeStart request was DEFINITIVELY rejected (pause/delete/fencing)
// after start_intent was persisted. A definitive rejection means the Scheduler
// transaction created no grant, and MarkRunning — the only path to beginning
// the turn — requires a grant, so a not_started stop proof is valid.
func (s *Store) FinishScheduledReceiptStartRejected(ctx context.Context, occurrenceID string, generation uint64, workerGeneration, status, reason, lastError string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_execution_receipts
SET status = ?, reason = ?, last_error = NULLIF(?, ''), finished_at = ?, lease_until = NULL,
    stop_proof_present = 1, stop_proof_generation = ?, stop_proof_kind = ?, stop_proof_at = ?
WHERE occurrence_id = ? AND receipt_generation = ? AND worker_generation = ? AND status = 'start_intent'`,
		status, reason, lastError, now.UTC(), generation, model.ScheduledStopProofNotStarted, now.UTC(),
		occurrenceID, generation, workerGeneration)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MarkScheduledReceiptRunning records the received grant and the logical start
// time. It succeeds only for the current generation/worker that already
// persisted start_intent.
func (s *Store) MarkScheduledReceiptRunning(ctx context.Context, occurrenceID string, generation uint64, workerGeneration string, grant model.ScheduledStartGrant) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_execution_receipts
SET status = 'running', started_at = ?, grant_generation = ?, grant_deadline = ?, lease_until = NULL
WHERE occurrence_id = ? AND receipt_generation = ? AND worker_generation = ? AND status = 'start_intent'`,
		grant.StartedAt.UTC(), grant.Generation, grant.Deadline.UTC(), occurrenceID, generation, workerGeneration)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FinishScheduledReceipt writes the terminal fact with a positive stop proof.
// It is called only AFTER the final message batch is durable, and only by the
// current generation/worker.
func (s *Store) FinishScheduledReceipt(ctx context.Context, occurrenceID string, generation uint64, workerGeneration, status, reason, lastError string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_execution_receipts
SET status = ?, reason = ?, last_error = NULLIF(?, ''), finished_at = ?, lease_until = NULL,
    stop_proof_present = 1, stop_proof_generation = ?, stop_proof_kind = ?, stop_proof_at = ?
WHERE occurrence_id = ? AND receipt_generation = ? AND worker_generation = ?
  AND status IN ('preparing','start_intent','running','uncertain') AND stop_proof_present = 0`,
		status, reason, lastError, now.UTC(), generation, model.ScheduledStopProofTerminal, now.UTC(),
		occurrenceID, generation, workerGeneration)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FinishScheduledReceiptBeforeStart writes a terminal fact for a rejection
// that never crossed start_intent. No side effect could have happened, so the
// stop proof is not_started.
func (s *Store) FinishScheduledReceiptBeforeStart(ctx context.Context, occurrenceID string, generation uint64, workerGeneration, status, reason, lastError string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_execution_receipts
SET status = ?, reason = ?, last_error = NULLIF(?, ''), finished_at = ?, lease_until = NULL,
    stop_proof_present = 1, stop_proof_generation = ?, stop_proof_kind = ?, stop_proof_at = ?
WHERE occurrence_id = ? AND receipt_generation = ? AND worker_generation = ?
  AND status IN ('accepted','preparing')`,
		status, reason, lastError, now.UTC(), generation, model.ScheduledStopProofNotStarted, now.UTC(),
		occurrenceID, generation, workerGeneration)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MarkScheduledReceiptUncertain records that the execution may still be
// running. It never overwrites a terminal/stopped receipt.
func (s *Store) MarkScheduledReceiptUncertain(ctx context.Context, occurrenceID string, generation uint64, reason, lastError string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_execution_receipts
SET status = 'uncertain', reason = ?, last_error = NULLIF(?, ''), lease_until = NULL
WHERE occurrence_id = ? AND receipt_generation = ? AND status IN ('start_intent','running') AND stop_proof_present = 0`,
		reason, lastError, occurrenceID, generation)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RecordScheduledStopProof is the controlled reconciliation write: it converts
// an uncertain receipt into terminal only with a generation-bound positive
// stop proof.
func (s *Store) RecordScheduledStopProof(ctx context.Context, occurrenceID string, generation uint64, kind string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_execution_receipts
SET status = 'interrupted', finished_at = ?, lease_until = NULL,
    stop_proof_present = 1, stop_proof_generation = ?, stop_proof_kind = ?, stop_proof_at = ?
WHERE occurrence_id = ? AND receipt_generation = ? AND status = 'uncertain' AND stop_proof_present = 0`,
		now.UTC(), generation, kind, now.UTC(), occurrenceID, generation)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CountActiveScheduledReceipts reports the managed-capacity occupancy
// (including uncertain executions, which are not proven stopped).
func (s *Store) CountActiveScheduledReceipts(ctx context.Context) (int, error) {
	var n int
	err := s.db.GetContext(ctx, &n, `SELECT COUNT(*) FROM scheduled_execution_receipts
WHERE status IN ('preparing','start_intent','running','uncertain')`)
	return n, err
}

// withNamedLock serializes a critical section across Runtime replicas sharing
// the MySQL instance.
func (s *Store) withNamedLock(ctx context.Context, name string, waitSeconds int, fn func(*sqlx.Conn) error) error {
	conn, err := s.db.Connx(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var acquired sql.NullInt64
	if err := conn.GetContext(ctx, &acquired, "SELECT GET_LOCK(?, ?)", name, waitSeconds); err != nil {
		return err
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return fmt.Errorf("mysql: named lock %q timeout", name)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(releaseCtx, "SELECT RELEASE_LOCK(?)", name)
	}
	defer release()
	return fn(conn)
}
