package mysql

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
)

// getUserLLMCredentialSQL looks up a user's credential row by primary key.
const getUserLLMCredentialSQL = `
SELECT user_id, api_key, update_time, create_time
FROM user_llm_credentials
WHERE user_id = ?
LIMIT 1
`

// upsertUserLLMCredentialSQL inserts or overwrites a user's token row
// (user-llm-token: saving is an idempotent overwrite; update_time refreshes
// via ON UPDATE CURRENT_TIMESTAMP).
const upsertUserLLMCredentialSQL = `
INSERT INTO user_llm_credentials (user_id, api_key)
VALUES (:user_id, :api_key)
ON DUPLICATE KEY UPDATE api_key = VALUES(api_key)
`

// deleteUserLLMCredentialSQL removes a user's token row. Deleting a missing
// row succeeds (DELETE affects zero rows without error).
const deleteUserLLMCredentialSQL = `
DELETE FROM user_llm_credentials
WHERE user_id = ?
`

// GetUserLLMCredential returns the user's stored LLM credential, or
// (nil, nil) when the user has none configured. Any other driver error is
// returned verbatim.
func (s *Store) GetUserLLMCredential(ctx context.Context, userID string) (*model.UserLLMCredential, error) {
	logQuery(ctx, "user_llm_credential.get", getUserLLMCredentialSQL, userID)

	var cred model.UserLLMCredential
	err := s.db.GetContext(ctx, &cred, getUserLLMCredentialSQL, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cred, nil
}

// UpsertUserLLMCredential stores the credential row, overwriting any previous
// token for the same user.
func (s *Store) UpsertUserLLMCredential(ctx context.Context, cred model.UserLLMCredential) error {
	logQuery(ctx, "user_llm_credential.upsert", upsertUserLLMCredentialSQL, cred.UserID)
	if _, err := sqlx.NamedExecContext(ctx, s.db, upsertUserLLMCredentialSQL, cred); err != nil {
		return err
	}
	return nil
}

// DeleteUserLLMCredential removes the user's credential row. It succeeds when
// no row exists — the caller's observable state ("no token configured") is
// identical either way.
func (s *Store) DeleteUserLLMCredential(ctx context.Context, userID string) error {
	logQuery(ctx, "user_llm_credential.delete", deleteUserLLMCredentialSQL, userID)
	if _, err := s.db.ExecContext(ctx, deleteUserLLMCredentialSQL, userID); err != nil {
		return err
	}
	return nil
}

// UserLLMToken satisfies agent.UserLLMTokenStore: it returns the user's
// configured token, or "" when the user has none. The resolver treats "" as
// "fall back to the global openai.api_key" and any error as a hard failure
// (never a silent credential swap).
func (s *Store) UserLLMToken(ctx context.Context, userID string) (string, error) {
	cred, err := s.GetUserLLMCredential(ctx, userID)
	if err != nil {
		return "", err
	}
	if cred == nil {
		return "", nil
	}
	return cred.APIKey, nil
}
