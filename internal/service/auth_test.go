package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/jwt"
)

// fakeUserStore is an in-memory UserStore for service tests. It is indexed by
// username and returns (nil, nil) for unknown users, matching the contract of
// the real mysql.Store.
type fakeUserStore struct {
	users map[string]*model.User
}

func (f *fakeUserStore) GetUserByUsername(_ context.Context, username string) (*model.User, error) {
	if u, ok := f.users[username]; ok {
		return u, nil
	}
	return nil, nil
}

const testSecret = "test-secret"
const testExpire = time.Hour

func mustHash(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	return string(h)
}

func newSvc(t *testing.T, users ...*model.User) (*AuthService, *fakeUserStore) {
	t.Helper()
	store := &fakeUserStore{users: map[string]*model.User{}}
	for _, u := range users {
		store.users[u.Username] = u
	}
	return NewAuthService(store, testSecret, testExpire), store
}

func TestLogin_Success(t *testing.T) {
	const (
		username = "alice"
		password = "correct-horse"
		userID   = "11111111-1111-1111-1111-111111111111"
	)
	svc, _ := newSvc(t, &model.User{
		UserID:   userID,
		Username: username,
		Password: mustHash(t, password),
		Status:   model.UserStatusActive,
	})

	before := time.Now()
	token, expireAt, err := svc.Login(context.Background(), username, password)
	require.NoError(t, err)

	assert.NotEmpty(t, token)

	gotUserID, verifyErr := jwt.Verify(testSecret, token)
	require.NoError(t, verifyErr, "issued token must verify")
	assert.Equal(t, userID, gotUserID)

	assert.InDelta(t, before.Add(testExpire).Unix(), expireAt.Unix(), 2,
		"expireAt should be ~now+expire")
	assert.WithinDuration(t, time.Now().Add(testExpire), expireAt, 2*time.Second)
}

func TestLogin_UserNotFound(t *testing.T) {
	svc, _ := newSvc(t)

	_, _, err := svc.Login(context.Background(), "ghost", "whatever")
	assert.ErrorIs(t, err, ErrUserNotFound)
}

func TestLogin_InvalidPassword(t *testing.T) {
	const (
		username = "bob"
		userID   = "22222222-2222-2222-2222-222222222222"
	)
	svc, _ := newSvc(t, &model.User{
		UserID:   userID,
		Username: username,
		Password: mustHash(t, "right-password"),
		Status:   model.UserStatusActive,
	})

	_, _, err := svc.Login(context.Background(), username, "wrong-password")
	assert.ErrorIs(t, err, ErrInvalidCredentials)

	// Sanity: errors.Is must not match the sibling sentinels.
	assert.NotErrorIs(t, err, ErrUserNotFound)
	assert.NotErrorIs(t, err, ErrUserDisabled)
}

func TestLogin_DisabledUser(t *testing.T) {
	const (
		username = "carol"
		userID   = "33333333-3333-3333-3333-333333333333"
	)
	svc, _ := newSvc(t, &model.User{
		UserID:   userID,
		Username: username,
		Password: mustHash(t, "any"),
		Status:   model.UserStatusDisabled,
	})

	_, _, err := svc.Login(context.Background(), username, "any")
	assert.ErrorIs(t, err, ErrUserDisabled)
}

func TestLogin_ActualBcryptHash(t *testing.T) {
	// Guard against an accidental regression where the service stores the
	// plaintext password or compares with a constant-time helper that bypasses
	// bcrypt. We seed a *real* bcrypt hash and confirm both the success and
	// failure paths react to bcrypt's comparison outcome.
	const (
		username = "dave"
		password = "s3cret-bcrypt"
		userID   = "44444444-4444-4444-4444-444444444444"
	)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	require.NotEqual(t, password, string(hash), "hash must not equal plaintext")

	svc, _ := newSvc(t, &model.User{
		UserID:   userID,
		Username: username,
		Password: string(hash),
		Status:   model.UserStatusActive,
	})

	// Correct password verifies and yields a token whose user_id matches.
	token, _, err := svc.Login(context.Background(), username, password)
	require.NoError(t, err)
	gotUserID, err := jwt.Verify(testSecret, token)
	require.NoError(t, err)
	assert.Equal(t, userID, gotUserID)

	// A single-character mutation must flip the result to ErrInvalidCredentials,
	// proving the comparison actually runs the hash through bcrypt.
	_, _, err = svc.Login(context.Background(), username, password+"x")
	assert.ErrorIs(t, err, ErrInvalidCredentials)
}
