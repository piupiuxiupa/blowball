package mysql

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

// setupFileVersionStore opens a connection and creates an isolated
// file_versions table mirroring migration 019. Tests are skipped when
// MYSQL_TEST_DSN is not set.
func setupFileVersionStore(t *testing.T) (*Store, func()) {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL-backed store test")
	}

	store, err := New(dsn)
	require.NoError(t, err)

	cleanup := func() {
		_, _ = store.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS file_versions")
		_ = store.Close()
	}

	_, err = store.db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS file_versions (
			id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
			user_id     VARCHAR(64)   NOT NULL,
			path        VARCHAR(1024) NOT NULL,
			version_id  VARCHAR(64)   NOT NULL,
			size        BIGINT        NOT NULL,
			mime        VARCHAR(255)  NOT NULL DEFAULT '',
			sha256      CHAR(64)      NOT NULL,
			create_time TIMESTAMP(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			PRIMARY KEY (id),
			UNIQUE KEY uk_fv_user_path_version (user_id, path(255), version_id),
			KEY idx_fv_user_path_time (user_id, path(255), create_time),
			UNIQUE KEY uk_fv_version_id (version_id)
		)`)
	require.NoError(t, err)
	_, _ = store.db.ExecContext(context.Background(), "TRUNCATE TABLE file_versions")

	return store, cleanup
}

func TestFileVersionStore_InsertLatestAsOf(t *testing.T) {
	if os.Getenv("MYSQL_TEST_DSN") == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL-backed store test")
	}
	store, cleanup := setupFileVersionStore(t)
	defer cleanup()
	ctx := context.Background()

	older := model.FileVersion{UserID: "u1", Path: "reports/a.md", VersionID: "v-1", Size: 3, Mime: "text/markdown", SHA256: "aa"}
	require.NoError(t, store.InsertVersion(ctx, older))
	time.Sleep(5 * time.Millisecond) // TIMESTAMP(3) ordering
	newer := model.FileVersion{UserID: "u1", Path: "reports/a.md", VersionID: "v-2", Size: 4, Mime: "text/markdown", SHA256: "bb"}
	require.NoError(t, store.InsertVersion(ctx, newer))
	// Another user's row must never leak into u1's results.
	require.NoError(t, store.InsertVersion(ctx, model.FileVersion{UserID: "u2", Path: "reports/a.md", VersionID: "v-9", Size: 1, SHA256: "cc"}))

	latest, err := store.LatestVersion(ctx, "u1", "reports/a.md")
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, "v-2", latest.VersionID)

	asOf, err := store.VersionAsOf(ctx, "u1", "reports/a.md", latest.CreateTime.Add(-time.Millisecond))
	require.NoError(t, err)
	require.NotNil(t, asOf)
	assert.Equal(t, "v-1", asOf.VersionID)

	byID, err := store.VersionByID(ctx, "v-9")
	require.NoError(t, err)
	require.NotNil(t, byID)
	assert.Equal(t, "u2", byID.UserID)

	none, err := store.LatestVersion(ctx, "u1", "never.md")
	require.NoError(t, err)
	assert.Nil(t, none)

	none, err = store.VersionByID(ctx, "nope")
	require.NoError(t, err)
	assert.Nil(t, none)
}
