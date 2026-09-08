package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/lush/blowball/internal/model"
)

// MaxLLMTokenLength bounds a stored LLM token; it mirrors the
// user_llm_credentials.api_key column width (VARCHAR(512) counts characters).
const MaxLLMTokenLength = 512

// ErrInvalidLLMToken marks a token rejected by Save's validation (blank after
// trimming, or longer than MaxLLMTokenLength). The HTTP layer maps it to 400.
var ErrInvalidLLMToken = errors.New("invalid llm token")

// LLMTokenStore is the persistence subset LLMTokenService needs. It is
// satisfied by *mysql.Store.
type LLMTokenStore interface {
	GetUserLLMCredential(ctx context.Context, userID string) (*model.UserLLMCredential, error)
	UpsertUserLLMCredential(ctx context.Context, cred model.UserLLMCredential) error
	DeleteUserLLMCredential(ctx context.Context, userID string) error
}

// LLMTokenService manages the per-user LLM gateway token (user-llm-token
// capability): save (validated, idempotent overwrite), delete (fall back to
// the global openai.api_key), and a status view that never exposes the token
// itself — only whether one is configured plus an unrecoverable masked
// preview. HTTP parsing stays in the handler; this layer owns validation and
// the masking rule.
type LLMTokenService struct {
	store LLMTokenStore
}

// NewLLMTokenService wires the service over its store.
func NewLLMTokenService(store LLMTokenStore) *LLMTokenService {
	return &LLMTokenService{store: store}
}

// Status reports whether the user has a token configured and, when so, its
// masked preview. A storage error is returned verbatim.
func (s *LLMTokenService) Status(ctx context.Context, userID string) (configured bool, masked string, err error) {
	cred, err := s.store.GetUserLLMCredential(ctx, userID)
	if err != nil {
		return false, "", err
	}
	if cred == nil {
		return false, "", nil
	}
	return true, MaskLLMToken(cred.APIKey), nil
}

// Save validates, trims and stores the token, overwriting any previous value,
// and returns the masked preview of the stored token.
func (s *LLMTokenService) Save(ctx context.Context, userID, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("%w: token is required", ErrInvalidLLMToken)
	}
	if utf8.RuneCountInString(token) > MaxLLMTokenLength {
		return "", fmt.Errorf("%w: token exceeds %d characters", ErrInvalidLLMToken, MaxLLMTokenLength)
	}
	if err := s.store.UpsertUserLLMCredential(ctx, model.UserLLMCredential{
		UserID: userID,
		APIKey: token,
	}); err != nil {
		return "", err
	}
	return MaskLLMToken(token), nil
}

// Delete removes the user's token; the next LLM call falls back to the global
// openai.api_key. Deleting an absent token succeeds.
func (s *LLMTokenService) Delete(ctx context.Context, userID string) error {
	return s.store.DeleteUserLLMCredential(ctx, userID)
}

// MaskLLMToken renders an unrecoverable preview: a fixed mask plus the last
// four characters. Tokens of eight characters or fewer show only the mask —
// for short secrets a suffix could reveal most (or all) of the value, which
// the spec's "cannot reconstruct the token" scenario forbids.
func MaskLLMToken(token string) string {
	if utf8.RuneCountInString(token) <= 8 {
		return "***"
	}
	return "***" + token[utf8.RuneCountInString(token)-4:]
}
