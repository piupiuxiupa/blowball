package mysql

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/lush/blowball/internal/model"
)

// EnsureResourceDraft idempotently creates the task's draft fence row.
func (s *Store) EnsureResourceDraft(ctx context.Context, userID, taskID string) (*model.ScheduledResourceDraft, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO scheduled_resource_drafts (task_id, user_id, draft_revision, generation)
VALUES (?, ?, 0, '')
ON DUPLICATE KEY UPDATE task_id = task_id`, taskID, userID)
	if err != nil {
		return nil, err
	}
	return s.GetResourceDraft(ctx, userID, taskID)
}

// GetResourceDraft returns the fence row for the owner, or (nil, nil).
func (s *Store) GetResourceDraft(ctx context.Context, userID, taskID string) (*model.ScheduledResourceDraft, error) {
	var d model.ScheduledResourceDraft
	err := s.db.GetContext(ctx, &d, `SELECT task_id, user_id, draft_revision, generation, update_time, create_time
FROM scheduled_resource_drafts WHERE task_id = ? AND user_id = ?`, taskID, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// BumpResourceDraft advances the fence after an authorized draft write.
func (s *Store) BumpResourceDraft(ctx context.Context, userID, taskID string) (uint64, error) {
	generation := randomGeneration()
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_resource_drafts
SET draft_revision = draft_revision + 1, generation = ?, update_time = NOW(3)
WHERE task_id = ? AND user_id = ?`, generation, taskID, userID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("mysql.BumpResourceDraft: draft row missing")
	}
	var revision uint64
	if err := s.db.GetContext(ctx, &revision, `SELECT draft_revision FROM scheduled_resource_drafts WHERE task_id = ?`, taskID); err != nil {
		return 0, err
	}
	return revision, nil
}

// NextResourceRevision allocates the next immutable revision. Callers hold the
// per-task resource lock so two publishers cannot pick the same number.
func (s *Store) NextResourceRevision(ctx context.Context, taskID string) (uint64, error) {
	var revision uint64
	err := s.db.GetContext(ctx, &revision, `SELECT COALESCE(MAX(resource_revision), 0) + 1
FROM scheduled_resource_versions WHERE task_id = ?`, taskID)
	return revision, err
}

// FindResourceVersionByRequest resolves the publication idempotency key.
func (s *Store) FindResourceVersionByRequest(ctx context.Context, userID, taskID, requestID string) (*model.ScheduledResourceVersion, error) {
	var v model.ScheduledResourceVersion
	err := s.db.GetContext(ctx, &v, `SELECT task_id, resource_revision, user_id, manifest_hash, file_count,
       total_bytes, manifest_json, request_id, create_time
FROM scheduled_resource_versions WHERE task_id = ? AND user_id = ? AND request_id = ?`, taskID, userID, requestID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// InsertResourceVersion inserts immutable publication metadata. A duplicate
// request_id returns (false, nil) so the caller can resolve the existing row.
func (s *Store) InsertResourceVersion(ctx context.Context, v model.ScheduledResourceVersion) (bool, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO scheduled_resource_versions
    (task_id, resource_revision, user_id, manifest_hash, file_count, total_bytes, manifest_json, request_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		v.TaskID, v.Revision, v.UserID, v.ManifestHash, v.FileCount, v.TotalBytes, v.ManifestJSON, v.RequestID)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// GetResourceVersion returns one owner/task-scoped immutable version.
func (s *Store) GetResourceVersion(ctx context.Context, userID, taskID string, revision uint64) (*model.ScheduledResourceVersion, error) {
	var v model.ScheduledResourceVersion
	err := s.db.GetContext(ctx, &v, `SELECT task_id, resource_revision, user_id, manifest_hash, file_count,
       total_bytes, manifest_json, request_id, create_time
FROM scheduled_resource_versions WHERE task_id = ? AND user_id = ? AND resource_revision = ?`,
		taskID, userID, revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// ListResourceVersions returns the task's immutable versions newest-first.
func (s *Store) ListResourceVersions(ctx context.Context, userID, taskID string) ([]model.ScheduledResourceVersion, error) {
	var out []model.ScheduledResourceVersion
	err := s.db.SelectContext(ctx, &out, `SELECT task_id, resource_revision, user_id, manifest_hash, file_count,
       total_bytes, manifest_json, request_id, create_time
FROM scheduled_resource_versions WHERE task_id = ? AND user_id = ?
ORDER BY resource_revision DESC`, taskID, userID)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteResourceVersion removes one unreferenced version's metadata. It is
// only called after the Scheduler's reference check.
func (s *Store) DeleteResourceVersion(ctx context.Context, userID, taskID string, revision uint64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM scheduled_resource_versions
WHERE task_id = ? AND user_id = ? AND resource_revision = ?`, taskID, userID, revision)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// WithinTaskResourceLock serializes publication and draft-maintenance paths
// across API nodes sharing the MySQL instance.
func (s *Store) WithinTaskResourceLock(ctx context.Context, taskID string, fn func() error) error {
	conn, err := s.db.Connx(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	lockName := "blowball:cron:" + taskID
	var acquired sql.NullInt64
	if err := conn.GetContext(ctx, &acquired, "SELECT GET_LOCK(?, ?)", lockName, 5); err != nil {
		return err
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return fmt.Errorf("mysql: resource lock timeout for task %s", taskID)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(releaseCtx, "SELECT RELEASE_LOCK(?)", lockName)
	}
	defer release()
	return fn()
}

func randomGeneration() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
