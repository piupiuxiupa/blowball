package mysql

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
)

// createUserSQL inserts a new user row. user_id is supplied by the caller
// (UUID generated upstream) so the user record is addressable before/after the
// insert without an extra round trip.
const createUserSQL = `
INSERT INTO users (user_id, username, password, status)
VALUES (:user_id, :username, :password, :status)
`

// getUserByUsernameSQL looks up a single user by its unique username.
const getUserByUsernameSQL = `
SELECT user_id, username, password, status, update_time, create_time
FROM users
WHERE username = ?
LIMIT 1
`

// getUserByIDSQL looks up a single user by its primary key.
const getUserByIDSQL = `
SELECT user_id, username, password, status, update_time, create_time
FROM users
WHERE user_id = ?
LIMIT 1
`

// CreateUser inserts u into the users table. The call fails if the username is
// already taken (callers should treat a duplicate-key error as a conflict).
func (s *Store) CreateUser(ctx context.Context, u model.User) error {
	logQuery(ctx, "user.create", createUserSQL)
	_, err := sqlx.NamedExecContext(ctx, s.db, createUserSQL, u)
	if err != nil {
		return err
	}
	return nil
}

// GetUserByUsername returns the user matching the given username, or
// (nil, nil) when no such user exists. Any other driver error is returned
// verbatim.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*model.User, error) {
	logQuery(ctx, "user.get_by_username", getUserByUsernameSQL, username)

	var u model.User
	err := s.db.GetContext(ctx, &u, getUserByUsernameSQL, username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByID returns the user matching the given user_id, or (nil, nil) when
// no such user exists.
func (s *Store) GetUserByID(ctx context.Context, userID string) (*model.User, error) {
	logQuery(ctx, "user.get_by_id", getUserByIDSQL, userID)

	var u model.User
	err := s.db.GetContext(ctx, &u, getUserByIDSQL, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}
