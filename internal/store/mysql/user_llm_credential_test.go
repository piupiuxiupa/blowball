package mysql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

// setupCredentialTestStore opens a connection and creates the credentials
// table. Tests are skipped when MYSQL_TEST_DSN is not set (same convention as
// the other MySQL-backed store tests).
func setupCredentialTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL-backed store test")
	}

	store, err := New(dsn)
	require.NoError(t, err)

	cleanup := func() {
		_, _ = store.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS user_llm_credentials")
		_ = store.Close()
	}
	_, err = store.db.ExecContext(context.Background(), `
CREATE TABLE user_llm_credentials (
    user_id     VARCHAR(64)  NOT NULL,
    api_key     VARCHAR(512) NOT NULL,
    update_time TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    create_time TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	require.NoError(t, err)
	return store, cleanup
}

func TestUserLLMCredential_RoundTrip(t *testing.T) {
	store, cleanup := setupCredentialTestStore(t)
	defer cleanup()
	ctx := context.Background()

	// Miss before any write.
	cred, err := store.GetUserLLMCredential(ctx, "user-a")
	require.NoError(t, err)
	assert.Nil(t, cred)
	token, err := store.UserLLMToken(ctx, "user-a")
	require.NoError(t, err)
	assert.Empty(t, token)

	// Insert.
	require.NoError(t, store.UpsertUserLLMCredential(ctx, model.UserLLMCredential{
		UserID: "user-a",
		APIKey: "sk-first",
	}))
	cred, err = store.GetUserLLMCredential(ctx, "user-a")
	require.NoError(t, err)
	require.NotNil(t, cred)
	assert.Equal(t, "sk-first", cred.APIKey)

	// Overwrite is idempotent and keeps exactly one row.
	require.NoError(t, store.UpsertUserLLMCredential(ctx, model.UserLLMCredential{
		UserID: "user-a",
		APIKey: "sk-second",
	}))
	cred, err = store.GetUserLLMCredential(ctx, "user-a")
	require.NoError(t, err)
	require.NotNil(t, cred)
	assert.Equal(t, "sk-second", cred.APIKey)
	token, err = store.UserLLMToken(ctx, "user-a")
	require.NoError(t, err)
	assert.Equal(t, "sk-second", token)
}

func TestUserLLMCredential_DeleteThenMiss(t *testing.T) {
	store, cleanup := setupCredentialTestStore(t)
	defer cleanup()
	ctx := context.Background()

	require.NoError(t, store.UpsertUserLLMCredential(ctx, model.UserLLMCredential{
		UserID: "user-b",
		APIKey: "sk-gone",
	}))
	require.NoError(t, store.DeleteUserLLMCredential(ctx, "user-b"))

	cred, err := store.GetUserLLMCredential(ctx, "user-b")
	require.NoError(t, err)
	assert.Nil(t, cred)

	// Deleting a missing row is a success.
	require.NoError(t, store.DeleteUserLLMCredential(ctx, "user-b"))
}

func TestUserLLMCredential_UserIsolation(t *testing.T) {
	store, cleanup := setupCredentialTestStore(t)
	defer cleanup()
	ctx := context.Background()

	require.NoError(t, store.UpsertUserLLMCredential(ctx, model.UserLLMCredential{
		UserID: "user-a",
		APIKey: "sk-a",
	}))
	require.NoError(t, store.UpsertUserLLMCredential(ctx, model.UserLLMCredential{
		UserID: "user-c",
		APIKey: "sk-c",
	}))

	token, err := store.UserLLMToken(ctx, "user-a")
	require.NoError(t, err)
	assert.Equal(t, "sk-a", token)
	token, err = store.UserLLMToken(ctx, "user-c")
	require.NoError(t, err)
	assert.Equal(t, "sk-c", token)
	token, err = store.UserLLMToken(ctx, "user-unset")
	require.NoError(t, err)
	assert.Empty(t, token)
}
