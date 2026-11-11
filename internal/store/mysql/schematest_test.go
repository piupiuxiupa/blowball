package mysql

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testSchemaLock serializes MySQL-backed test setups across packages running
// in parallel (go test runs packages concurrently; all of them share one test
// schema). Held for the whole test so another package cannot drop/recreate the
// tables mid-test.
const testSchemaLock = "blowball_test_store_schema"

func lockTestSchema(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	conn, err := store.db.Connx(ctx)
	require.NoError(t, err)
	var acquired sql.NullInt64
	require.NoError(t, conn.GetContext(ctx, &acquired, "SELECT GET_LOCK(?, 60)", testSchemaLock))
	require.True(t, acquired.Valid && acquired.Int64 == 1, "could not acquire the MySQL test schema lock")
	t.Cleanup(func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(releaseCtx, "SELECT RELEASE_LOCK(?)", testSchemaLock)
		_ = conn.Close()
	})
}
