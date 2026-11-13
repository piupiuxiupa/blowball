package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/jwt"
)

func TestExecutionTokenIssuerIssuesBoundedUserIdentity(t *testing.T) {
	issuer := NewExecutionTokenIssuer("test-secret", time.Minute)
	token, err := issuer.Issue(context.Background(), "user-1")
	require.NoError(t, err)
	require.NotEmpty(t, token)
	userID, err := jwt.Verify("test-secret", token)
	require.NoError(t, err)
	require.Equal(t, "user-1", userID)
}

func TestExecutionTokenIssuerRejectsUnboundedLifetime(t *testing.T) {
	issuer := NewExecutionTokenIssuer("test-secret", 0)
	_, err := issuer.Issue(context.Background(), "user-1")
	require.Error(t, err)
}
