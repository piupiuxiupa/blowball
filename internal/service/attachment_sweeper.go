package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
)

// Staged attachment orphan-sweep constants (add-staged-attachment-upload).
// Staged uploads never get a session-delete cleanup, so the TTL sweep is the
// only reclamation path for "uploaded but never sent" images.
const (
	// StagedAttachmentTTL is how long a staged attachment may sit unbound
	// before the sweeper reclaims it. 24h comfortably covers "upload during
	// a bad-network evening, send tomorrow"; the send path claims instantly.
	StagedAttachmentTTL = 24 * time.Hour
	// stagedSweepEvery is the sweep cadence.
	stagedSweepEvery = 10 * time.Minute
	// stagedSweepBatch caps one round's row count so a neglected deployment
	// drains its backlog over several rounds instead of one long DELETE.
	stagedSweepBatch = 200
)

// AttachmentSweepStore is the registry subset the sweeper needs; satisfied by
// *mysql.Store and fakes in tests.
type AttachmentSweepStore interface {
	ListExpiredStagedAttachments(ctx context.Context, cutoff time.Time, limit int) ([]model.Attachment, error)
	DeleteStagedAttachment(ctx context.Context, id string) (bool, error)
}

// AttachmentSweeper reclaims staged (uploaded-but-never-claimed) attachments
// past their TTL: delete the registry row, then the file. The row delete
// carries a session_id IS NULL guard, so a concurrent send-time claim always
// wins — a lost delete means the file moved on and MUST be left alone.
type AttachmentSweeper struct {
	store      AttachmentSweepStore
	stagingDir func(userID string) string
}

// NewAttachmentSweeper wires the sweeper with the registry store and the
// per-user staging-dir resolver (fs.Store.UserAttachmentStagingDir).
func NewAttachmentSweeper(store AttachmentSweepStore, stagingDir func(userID string) string) *AttachmentSweeper {
	return &AttachmentSweeper{store: store, stagingDir: stagingDir}
}

// Start runs the sweep loop until ctx is cancelled: one sweep immediately (a
// process restart may already have a backlog), then once per interval. Panics
// and store errors are logged and survived — the sweeper is best-effort by
// contract and must never take the server down.
func (s *AttachmentSweeper) Start(ctx context.Context) {
	s.SweepOnce(ctx)
	ticker := time.NewTicker(stagedSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SweepOnce(ctx)
		}
	}
}

// SweepOnce deletes up to one batch of expired staged rows and their files.
// Per-row failures are logged and skipped, never aborting the round.
func (s *AttachmentSweeper) SweepOnce(ctx context.Context) {
	cutoff := time.Now().Add(-StagedAttachmentTTL)
	rows, err := s.store.ListExpiredStagedAttachments(ctx, cutoff, stagedSweepBatch)
	if err != nil {
		logger.FromContext(ctx).Warn("staged attachment sweep: list failed",
			zap.String("op", "attachment.sweep"), zap.Error(err))
		return
	}
	for _, row := range rows {
		deleted, err := s.store.DeleteStagedAttachment(ctx, row.ID)
		if err != nil {
			logger.FromContext(ctx).Warn("staged attachment sweep: row delete failed",
				zap.String("op", "attachment.sweep"),
				zap.String("attachment_id", row.ID), zap.Error(err))
			continue
		}
		if !deleted {
			// The row was claimed (or already reaped) between list and
			// delete — its file now belongs to a session; leave it alone.
			continue
		}
		ext, ok := model.AttachmentMIMEExt[row.MIME]
		if !ok {
			continue
		}
		path := filepath.Join(s.stagingDir(row.UserID), row.ID+ext)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.FromContext(ctx).Warn("staged attachment sweep: file remove failed",
				zap.String("op", "attachment.sweep"),
				zap.String("attachment_id", row.ID),
				zap.String("path", path), zap.Error(err))
		}
	}
}
