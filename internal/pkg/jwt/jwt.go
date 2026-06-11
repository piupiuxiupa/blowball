// Package jwt signs and verifies HS256 JSON Web Tokens carrying a user_id.
package jwt

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is the registered claim set plus the blowball-specific user_id.
type Claims struct {
	UserID string `json:"user_id"`
	jwt.RegisteredClaims
}

// Sign issues a new HS256 token for the given userID that expires after
// expire. It returns the encoded token string.
func Sign(secret string, userID string, expire time.Duration) (string, error) {
	if secret == "" {
		return "", fmt.Errorf("jwt sign: secret must be non-empty")
	}
	if userID == "" {
		return "", fmt.Errorf("jwt sign: userID must be non-empty")
	}
	now := time.Now()
	claims := Claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(expire)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// Verify validates the tokenStr using secret and returns the user_id claim.
// It returns an error if the signature is invalid, the token is malformed,
// or it has expired.
func Verify(secret string, tokenStr string) (string, error) {
	if secret == "" {
		return "", fmt.Errorf("jwt verify: secret must be non-empty")
	}
	if tokenStr == "" {
		return "", fmt.Errorf("jwt verify: token must be non-empty")
	}

	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return "", fmt.Errorf("jwt verify: %w", err)
	}
	if !token.Valid {
		return "", fmt.Errorf("jwt verify: token invalid")
	}
	if claims.UserID == "" {
		return "", fmt.Errorf("jwt verify: missing user_id claim")
	}
	return claims.UserID, nil
}
