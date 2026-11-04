package service

import (
	"context"
	"fmt"
	"time"

	"github.com/lush/blowball/internal/pkg/jwt"
)

// ExecutionTokenIssuer is the trusted, internal-only authorization service for
// background turns. Tokens are returned to the caller's memory only; there is
// no HTTP endpoint and no storage. The scheduler performs owner/task/session
// authorization before calling Issue.
type ExecutionTokenIssuer struct {
	secret string
	ttl    time.Duration
}

func NewExecutionTokenIssuer(secret string, ttl time.Duration) *ExecutionTokenIssuer {
	return &ExecutionTokenIssuer{secret: secret, ttl: ttl}
}

func (i *ExecutionTokenIssuer) Issue(_ context.Context, userID string) (string, error) {
	if i == nil || i.ttl <= 0 {
		return "", fmt.Errorf("execution token issuer: positive ttl is required")
	}
	return jwt.Sign(i.secret, userID, i.ttl)
}
