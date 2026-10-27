package mysql

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

// setupAttachmentStore creates an isolated attachments table. Tests skip
// without MYSQL_TEST_DSN (the package-wide convention).
func setupAttachmentStore(t *testing.T) (*Store, func()) {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL-backed store test")
	}
	store, err := New(dsn)
	require.NoError(t, err)
	cleanup := func() {
		_, _ = store.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS attachments")
		_ = store.Close()
	}
	_, err = store.db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS attachments (
			id          CHAR(36)     NOT NULL,
			user_id     CHAR(36)     NOT NULL,
			session_id  CHAR(36)     NULL,
			filename    VARCHAR(255) NOT NULL,
			mime        VARCHAR(64)  NOT NULL,
			size        BIGINT       NOT NULL,
			message_id  BIGINT       NULL,
			create_time TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			PRIMARY KEY (id),
			KEY ix_attachments_session (session_id),
			KEY ix_attachments_user_session (user_id, session_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
	`)
	require.NoError(t, err)
	return store, cleanup
}

// strptr returns a *string for test fixtures (nullable session_id).
func strptr(s string) *string { return &s }

func TestAttachment_InsertAndGet_OwnershipTriple(t *testing.T) {
	store, cleanup := setupAttachmentStore(t)
	defer cleanup()
	ctx := context.Background()

	a := model.Attachment{
		ID: "01953c5e-7f9e-7cc8-8db5-2f4a5b6c7d8e", UserID: "u-1", SessionID: strptr("s-1"),
		Filename: "shot.png", MIME: "image/png", Size: 1234,
	}
	require.NoError(t, store.InsertAttachment(ctx, a))

	got, err := store.GetAttachment(ctx, a.ID, "u-1", "s-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, a.ID, got.ID)
	require.Equal(t, "shot.png", got.Filename)
	require.Equal(t, "image/png", got.MIME)
	require.Equal(t, int64(1234), got.Size)
	require.Nil(t, got.MessageID)

	// Wrong user / wrong session / unknown id all resolve to (nil, nil) —
	// the ownership-triple contract the send path 404s on.
	for _, tc := range [][3]string{
		{a.ID, "u-2", "s-1"},
		{a.ID, "u-1", "s-2"},
		{"no-such-id", "u-1", "s-1"},
	} {
		miss, err := store.GetAttachment(ctx, tc[0], tc[1], tc[2])
		require.NoError(t, err)
		require.Nil(t, miss, "triple %v must not resolve", tc)
	}
}

// TestAttachment_StagedLifecycle exercises the add-staged-attachment-upload
// store primitives: insert staged (NULL session) → quota count → by-user
// lookup → atomic claim → unclaim rollback → expiry listing → staged delete.
func TestAttachment_StagedLifecycle(t *testing.T) {
	store, cleanup := setupAttachmentStore(t)
	defer cleanup()
	ctx := context.Background()

	staged := model.Attachment{
		ID: "01953c5e-8000-7000-8000-000000000001", UserID: "u-1", SessionID: nil,
		Filename: "staged.png", MIME: "image/png", Size: 100,
	}
	require.NoError(t, store.InsertAttachment(ctx, staged))

	// Staged row: triple lookup (content endpoint) must NOT resolve it.
	miss, err := store.GetAttachment(ctx, staged.ID, "u-1", "s-1")
	require.NoError(t, err)
	require.Nil(t, miss, "staged row must not resolve via the session triple")

	// By-user lookup resolves and reports the staged state.
	got, err := store.GetAttachmentByUser(ctx, staged.ID, "u-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Nil(t, got.SessionID)
	foreign, err := store.GetAttachmentByUser(ctx, staged.ID, "u-2")
	require.NoError(t, err)
	require.Nil(t, foreign)

	// Quota counts staged rows only.
	n, err := store.CountStagedAttachments(ctx, "u-1")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// Claim: first wins, second loses — regardless of session.
	ok, err := store.ClaimAttachment(ctx, staged.ID, "u-1", "s-1")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = store.ClaimAttachment(ctx, staged.ID, "u-1", "s-2")
	require.NoError(t, err)
	require.False(t, ok, "already-claimed row must not be claimable again")

	// Claimed rows leave the staged quota and the expiry listing.
	n, err = store.CountStagedAttachments(ctx, "u-1")
	require.NoError(t, err)
	require.Equal(t, 0, n)

	// Unclaim rolls back only for the claiming session.
	require.NoError(t, store.UnclaimAttachment(ctx, staged.ID, "u-1", "s-2"))
	got, err = store.GetAttachmentByUser(ctx, staged.ID, "u-1")
	require.NoError(t, err)
	require.NotNil(t, got.SessionID, "wrong-session unclaim must be a no-op")
	require.NoError(t, store.UnclaimAttachment(ctx, staged.ID, "u-1", "s-1"))
	got, err = store.GetAttachmentByUser(ctx, staged.ID, "u-1")
	require.NoError(t, err)
	require.Nil(t, got.SessionID, "owning-session unclaim must restore staged")

	// Expiry listing: future cutoff lists it, past cutoff does not.
	expired, err := store.ListExpiredStagedAttachments(ctx, time.Now().Add(time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	fresh, err := store.ListExpiredStagedAttachments(ctx, time.Now().Add(-time.Hour), 10)
	require.NoError(t, err)
	require.Empty(t, fresh)

	// DeleteStagedAttachment: staged → deleted; re-delete reports false.
	deleted, err := store.DeleteStagedAttachment(ctx, staged.ID)
	require.NoError(t, err)
	require.True(t, deleted)
	deleted, err = store.DeleteStagedAttachment(ctx, staged.ID)
	require.NoError(t, err)
	require.False(t, deleted)

	// A failed file cleanup restores the expired row for the next sweep while
	// preserving its age so it is immediately retryable.
	staged.CreateTime = time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	require.NoError(t, store.RestoreStagedAttachment(ctx, staged))
	restored, err := store.GetAttachmentByUser(ctx, staged.ID, "u-1")
	require.NoError(t, err)
	require.NotNil(t, restored)
	require.Nil(t, restored.SessionID)
	assert.True(t, restored.CreateTime.Before(time.Now().Add(-24*time.Hour)))

	// A claimed row must survive DeleteStagedAttachment (claim-vs-sweep race).
	bound := model.Attachment{
		ID: "01953c5e-8000-7000-8000-000000000002", UserID: "u-1", SessionID: strptr("s-9"),
		Filename: "bound.png", MIME: "image/png", Size: 50,
	}
	require.NoError(t, store.InsertAttachment(ctx, bound))
	deleted, err = store.DeleteStagedAttachment(ctx, bound.ID)
	require.NoError(t, err)
	require.False(t, deleted)
	kept, err := store.GetAttachmentByUser(ctx, bound.ID, "u-1")
	require.NoError(t, err)
	require.NotNil(t, kept)
}

func TestInsertStagedAttachment_ConcurrentQuota(t *testing.T) {
	store, cleanup := setupAttachmentStore(t)
	defer cleanup()
	ctx := context.Background()
	const userID = "u-quota"

	for i := 0; i < 19; i++ {
		require.NoError(t, store.InsertAttachment(ctx, model.Attachment{
			ID:       fmt.Sprintf("existing-%02d", i),
			UserID:   userID,
			Filename: "existing.png",
			MIME:     "image/png",
			Size:     1,
		}))
	}

	type result struct {
		ok  bool
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ok, err := store.InsertStagedAttachment(ctx, model.Attachment{
				ID:       fmt.Sprintf("concurrent-%02d", i),
				UserID:   userID,
				Filename: "new.png",
				MIME:     "image/png",
				Size:     1,
			}, 20)
			results <- result{ok: ok, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	for result := range results {
		require.NoError(t, result.err)
		if result.ok {
			successes++
		}
	}
	assert.Equal(t, 1, successes, "exactly one concurrent upload may reserve the final slot")
	staged, err := store.CountStagedAttachments(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, 20, staged)
}

func TestRestoreStagedAttachment_RecoveryIsNeverQuotaRejected(t *testing.T) {
	store, cleanup := setupAttachmentStore(t)
	defer cleanup()
	ctx := context.Background()
	const userID = "u-restore-full"

	for i := 0; i < model.MaxStagedAttachmentsPerUser; i++ {
		require.NoError(t, store.InsertAttachment(ctx, model.Attachment{
			ID: fmt.Sprintf("restore-full-%02d", i), UserID: userID,
			Filename: "staged.png", MIME: "image/png", Size: 1,
		}))
	}
	recovery := model.Attachment{
		ID: "restore-full-retry", UserID: userID, Filename: "retry.png", MIME: "image/png",
		Size: 1, CreateTime: time.Now().Add(-48 * time.Hour),
	}

	require.NoError(t, store.RestoreStagedAttachment(ctx, recovery),
		"the file has already consumed disk and must retain a retryable registry row")

	restored, err := store.GetAttachmentByUser(ctx, recovery.ID, userID)
	require.NoError(t, err)
	require.NotNil(t, restored)
	assert.Nil(t, restored.SessionID)

	staged, err := store.CountStagedAttachments(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, model.MaxStagedAttachmentsPerUser+1, staged)

	reserved, err := store.InsertStagedAttachment(ctx, model.Attachment{
		ID: "restore-full-new", UserID: userID, Filename: "new.png", MIME: "image/png", Size: 1,
	}, model.MaxStagedAttachmentsPerUser)
	require.NoError(t, err)
	assert.False(t, reserved, "new uploads must stay blocked while recovery is above quota")
}

func TestStagedQuotaLock_CoversUnclaimAndRestore(t *testing.T) {
	store, cleanup := setupAttachmentStore(t)
	defer cleanup()
	ctx := context.Background()

	t.Run("unclaim", func(t *testing.T) {
		const userID = "u-unclaim"
		for i := 0; i < 19; i++ {
			require.NoError(t, store.InsertAttachment(ctx, model.Attachment{
				ID: fmt.Sprintf("unclaim-%02d", i), UserID: userID,
				Filename: "x.png", MIME: "image/png", Size: 1,
			}))
		}
		bound := model.Attachment{
			ID: "unclaim-bound", UserID: userID, SessionID: strptr("s-1"),
			Filename: "bound.png", MIME: "image/png", Size: 1,
		}
		require.NoError(t, store.InsertAttachment(ctx, bound))

		start := make(chan struct{})
		var wg sync.WaitGroup
		var inserted bool
		var insertErr error
		var unclaimErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			inserted, insertErr = store.InsertStagedAttachment(ctx, model.Attachment{
				ID: "unclaim-new", UserID: userID, Filename: "new.png", MIME: "image/png", Size: 1,
			}, 20)
		}()
		go func() {
			defer wg.Done()
			<-start
			unclaimErr = store.UnclaimAttachment(ctx, bound.ID, userID, "s-1")
		}()
		close(start)
		wg.Wait()

		require.NoError(t, insertErr)
		if inserted {
			assert.ErrorIs(t, unclaimErr, ErrStagedAttachmentQuotaExceeded)
		} else {
			require.NoError(t, unclaimErr)
		}
		staged, err := store.CountStagedAttachments(ctx, userID)
		require.NoError(t, err)
		assert.Equal(t, 20, staged)
	})

	t.Run("restore", func(t *testing.T) {
		const userID = "u-restore"
		for i := 0; i < 19; i++ {
			require.NoError(t, store.InsertAttachment(ctx, model.Attachment{
				ID: fmt.Sprintf("restore-%02d", i), UserID: userID,
				Filename: "x.png", MIME: "image/png", Size: 1,
			}))
		}
		restoreRow := model.Attachment{
			ID: "restore-retry", UserID: userID, Filename: "retry.png", MIME: "image/png", Size: 1,
			CreateTime: time.Now().Add(-48 * time.Hour),
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		var inserted bool
		var insertErr error
		var restoreErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			inserted, insertErr = store.InsertStagedAttachment(ctx, model.Attachment{
				ID: "restore-new", UserID: userID, Filename: "new.png", MIME: "image/png", Size: 1,
			}, 20)
		}()
		go func() {
			defer wg.Done()
			<-start
			restoreErr = store.RestoreStagedAttachment(ctx, restoreRow)
		}()
		close(start)
		wg.Wait()

		require.NoError(t, insertErr)
		require.NoError(t, restoreErr,
			"a sweeper recovery must never be rejected after its row was deleted")
		staged, err := store.CountStagedAttachments(ctx, userID)
		require.NoError(t, err)
		if inserted {
			assert.Equal(t, 21, staged,
				"a concurrent upload may reserve the final slot before the recovery lands")
		} else {
			assert.Equal(t, 20, staged,
				"once the recovery lands first, the concurrent upload must be rejected")
		}
		restored, err := store.GetAttachmentByUser(ctx, restoreRow.ID, userID)
		require.NoError(t, err)
		require.NotNil(t, restored)
		assert.Nil(t, restored.SessionID)

		reserved, err := store.InsertStagedAttachment(ctx, model.Attachment{
			ID: "restore-blocked", UserID: userID, Filename: "blocked.png", MIME: "image/png", Size: 1,
		}, 20)
		require.NoError(t, err)
		assert.False(t, reserved, "recovery may exceed the quota but must block new uploads")
	})
}
