package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

// fakeSweepStore mirrors the claim-vs-sweep contract: DeleteStagedAttachment
// only wins while the row is still staged.
type fakeSweepStore struct {
	rows map[string]model.Attachment
}

func (f *fakeSweepStore) ListExpiredStagedAttachments(_ context.Context, cutoff time.Time, limit int) ([]model.Attachment, error) {
	var out []model.Attachment
	for _, a := range f.rows {
		if a.SessionID == nil && a.CreateTime.Before(cutoff) {
			out = append(out, a)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeSweepStore) DeleteStagedAttachment(_ context.Context, id string) (bool, error) {
	a, ok := f.rows[id]
	if !ok || a.SessionID != nil {
		return false, nil
	}
	delete(f.rows, id)
	return true, nil
}

func (f *fakeSweepStore) RestoreStagedAttachment(_ context.Context, a model.Attachment) error {
	if _, exists := f.rows[a.ID]; !exists {
		f.rows[a.ID] = a
	}
	return nil
}

func TestAttachmentSweeper_ExpiredStagedReaped(t *testing.T) {
	root := t.TempDir()
	staging := func(userID string) string { return filepath.Join(root, userID, "attachments", "_staging") }

	old := time.Now().Add(-25 * time.Hour)
	store := &fakeSweepStore{rows: map[string]model.Attachment{
		"expired": {ID: "expired", UserID: "u-1", MIME: "image/png", CreateTime: old},
		"fresh":   {ID: "fresh", UserID: "u-1", MIME: "image/png", CreateTime: time.Now()},
		"bound":   {ID: "bound", UserID: "u-1", SessionID: ptr("s-1"), MIME: "image/png", CreateTime: old},
	}}
	require.NoError(t, os.MkdirAll(staging("u-1"), 0o755))
	for _, id := range []string{"expired", "fresh", "bound"} {
		require.NoError(t, os.WriteFile(filepath.Join(staging("u-1"), id+".png"), []byte("x"), 0o644))
	}

	NewAttachmentSweeper(store, staging).SweepOnce(context.Background())

	// Expired staged: row + file gone.
	_, ok := store.rows["expired"]
	assert.False(t, ok)
	_, err := os.Stat(filepath.Join(staging("u-1"), "expired.png"))
	assert.True(t, os.IsNotExist(err))

	// Fresh staged and bound rows survive with their files.
	assert.Contains(t, store.rows, "fresh")
	assert.Contains(t, store.rows, "bound")
	for _, id := range []string{"fresh", "bound"} {
		_, err := os.Stat(filepath.Join(staging("u-1"), id+".png"))
		assert.NoError(t, err, "%s file must survive", id)
	}
}

// TestAttachmentSweeper_MissingFileStillDeletesRow: an externally deleted
// file does not block the row delete or the rest of the round.
func TestAttachmentSweeper_MissingFileStillDeletesRow(t *testing.T) {
	root := t.TempDir()
	staging := func(userID string) string { return filepath.Join(root, userID, "attachments", "_staging") }
	old := time.Now().Add(-25 * time.Hour)
	store := &fakeSweepStore{rows: map[string]model.Attachment{
		"gone":    {ID: "gone", UserID: "u-1", MIME: "image/png", CreateTime: old},
		"expired": {ID: "expired", UserID: "u-1", MIME: "image/png", CreateTime: old},
	}}
	require.NoError(t, os.MkdirAll(staging("u-1"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staging("u-1"), "expired.png"), []byte("x"), 0o644))

	NewAttachmentSweeper(store, staging).SweepOnce(context.Background())

	assert.NotContains(t, store.rows, "gone", "row must be deleted even when the file is already missing")
	assert.NotContains(t, store.rows, "expired", "the round must continue past the missing file")
}

func TestAttachmentSweeper_FileDeleteFailureRestoresRetryRow(t *testing.T) {
	root := t.TempDir()
	staging := func(userID string) string { return filepath.Join(root, userID, "attachments", "_staging") }
	old := time.Now().Add(-25 * time.Hour)
	store := &fakeSweepStore{rows: map[string]model.Attachment{
		"retry": {ID: "retry", UserID: "u-1", MIME: "image/png", CreateTime: old},
	}}
	dir := staging("u-1")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "retry.png"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "retry.png", "blocker"), []byte("x"), 0o644))

	sweeper := NewAttachmentSweeper(store, staging)
	sweeper.SweepOnce(context.Background())

	row, ok := store.rows["retry"]
	require.True(t, ok, "failed file cleanup must restore an expired staged row for retry")
	assert.Equal(t, old, row.CreateTime)

	// The next sweep can finish once the obstruction is gone.
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "retry.png")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "retry.png"), []byte("x"), 0o644))
	sweeper.SweepOnce(context.Background())
	assert.NotContains(t, store.rows, "retry")
	_, err := os.Stat(filepath.Join(dir, "retry.png"))
	assert.True(t, os.IsNotExist(err))
}

// TestAttachmentSweeper_ClaimRaceKeepsFile: when the row delete loses the
// race against a send-time claim, the file belongs to the claiming session
// and must survive.
func TestAttachmentSweeper_ClaimRaceKeepsFile(t *testing.T) {
	root := t.TempDir()
	staging := func(userID string) string { return filepath.Join(root, userID, "attachments", "_staging") }
	old := time.Now().Add(-25 * time.Hour)
	store := &fakeSweepStore{rows: map[string]model.Attachment{
		"raced": {ID: "raced", UserID: "u-1", MIME: "image/png", CreateTime: old},
	}}
	require.NoError(t, os.MkdirAll(staging("u-1"), 0o755))
	path := filepath.Join(staging("u-1"), "raced.png")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	// Simulate the claim landing between list and delete.
	wrapped := &claimRacingStore{fakeSweepStore: store}

	NewAttachmentSweeper(wrapped, staging).SweepOnce(context.Background())

	row, ok := store.rows["raced"]
	assert.True(t, ok, "the guarded delete must lose the claim race — row survives")
	assert.NotNil(t, row.SessionID)
	_, err := os.Stat(path)
	assert.NoError(t, err, "a claimed row's file belongs to the session and must survive the sweep")
}

// claimRacingStore flips the row to claimed right when the sweeper tries to
// delete it, making the guarded delete lose the race.
type claimRacingStore struct{ *fakeSweepStore }

func (c *claimRacingStore) DeleteStagedAttachment(ctx context.Context, id string) (bool, error) {
	a := c.rows[id]
	a.SessionID = ptr("s-1")
	c.rows[id] = a
	return c.fakeSweepStore.DeleteStagedAttachment(ctx, id)
}

func ptr(s string) *string { return &s }
