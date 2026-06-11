// Package service holds the application's business logic. The auth service
// owns login: it looks a user up by username, verifies the supplied password
// against the stored bcrypt hash, and issues a JWT on success.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/jwt"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
)

// Sentinel errors. Handlers MUST distinguish these with errors.Is rather than
// string-matching the message, so the mapping from service outcome to HTTP
// response stays in exactly one place.
var (
	// ErrUserNotFound is returned by Login when no user matches the supplied
	// username.
	ErrUserNotFound = errors.New("user not found")
	// ErrInvalidCredentials is returned by Login when the supplied password
	// does not match the stored bcrypt hash.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrUserDisabled is returned by Login when the matched user's Status is
	// anything other than model.UserStatusActive.
	ErrUserDisabled = errors.New("user disabled")
)

// UserStore is the subset of the persistence layer that AuthService needs.
// Defining it here keeps the service decoupled from the concrete mysql.Store
// so it can be unit-tested with a trivial in-memory fake.
type UserStore interface {
	GetUserByUsername(ctx context.Context, username string) (*model.User, error)
}

// AuthService owns login: user lookup, bcrypt verification, status gating and
// JWT issuance.
type AuthService struct {
	userStore UserStore
	jwtSecret string
	jwtExpire time.Duration
}

// NewAuthService wires the service with its store and JWT parameters.
// jwtExpire is the lifetime baked into every token this service issues.
func NewAuthService(us UserStore, secret string, expire time.Duration) *AuthService {
	return &AuthService{
		userStore: us,
		jwtSecret: secret,
		jwtExpire: expire,
	}
}

// Login authenticates username/password and returns a freshly signed JWT plus
// the absolute time at which it expires. The bcrypt comparison path is taken
// for both existing users (real hash) and deliberately-missing users (empty
// hash) is NOT done here — the spec only mandates the sentinel errors, so we
// return ErrUserNotFound immediately when the lookup misses, which avoids
// touching bcrypt entirely and keeps the failure mode crisp.
//
// Passwords are never logged; only the outcome (success/failure) and the
// reason are recorded, always with the request trace_id attached.
func (s *AuthService) Login(ctx context.Context, username, password string) (string, time.Time, error) {
	tid := trace.FromContext(ctx)
	log := logger.L().With(zap.String("op", "auth.login"))
	if tid != "" {
		log = log.With(zap.String("trace_id", tid))
	}

	user, err := s.userStore.GetUserByUsername(ctx, username)
	if err != nil {
		log.Error("user lookup failed", zap.String("username", username), zap.Error(err))
		return "", time.Time{}, fmt.Errorf("auth.login: lookup: %w", err)
	}
	if user == nil {
		log.Info("login failed: user not found", zap.String("username", username))
		return "", time.Time{}, ErrUserNotFound
	}

	if user.Status != model.UserStatusActive {
		log.Info("login failed: user disabled", zap.String("username", username), zap.String("status", user.Status))
		return "", time.Time{}, ErrUserDisabled
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)); err != nil {
		log.Info("login failed: invalid credentials", zap.String("username", username))
		return "", time.Time{}, ErrInvalidCredentials
	}

	expireAt := time.Now().Add(s.jwtExpire)
	token, err := jwt.Sign(s.jwtSecret, user.UserID, s.jwtExpire)
	if err != nil {
		log.Error("jwt sign failed", zap.String("username", username), zap.Error(err))
		return "", time.Time{}, fmt.Errorf("auth.login: sign: %w", err)
	}

	log.Info("login succeeded", zap.String("username", username), zap.String("user_id", user.UserID))
	return token, expireAt, nil
}
