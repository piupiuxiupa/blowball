package mysql

import (
	"context"
	"testing"

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
			session_id  CHAR(36)     NOT NULL,
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

func TestAttachment_InsertAndGet_OwnershipTriple(t *testing.T) {
	store, cleanup := setupAttachmentStore(t)
	defer cleanup()
	ctx := context.Background()

	a := model.Attachment{
		ID: "01953c5e-7f9e-7cc8-8db5-2f4a5b6c7d8e", UserID: "u-1", SessionID: "s-1",
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
