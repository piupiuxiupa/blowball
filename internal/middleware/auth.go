package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	jwtpkg "github.com/golang-jwt/jwt/v5"

	"github.com/lush/blowball/internal/pkg/jwt"
)

// UserIDKey is the gin.Context key under which AuthMiddleware publishes the
// authenticated user_id (string) extracted from a verified Bearer token.
const UserIDKey = "user_id"

const authorizationHeader = "Authorization"
const bearerPrefix = "Bearer "

// authzErrorReason is the message string the spec mandates for each 401
// scenario. Keeping them as constants lets tests assert exact text without
// magic strings drifting between middleware and handler.
const (
	reasonMissingToken = "missing token"
	reasonExpiredToken = "token expired"
	reasonInvalidToken = "invalid token"
)

// AuthMiddleware returns a gin middleware that validates a Bearer JWT from the
// Authorization header. On success it publishes the resolved user_id on the
// gin.Context under UserIDKey (and backstops the trace_id when TraceMiddleware
// has not run). On failure it aborts the chain with the spec-mandated 401
// error body.
func AuthMiddleware(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader(authorizationHeader)
		token, ok := bearerToken(raw)
		if !ok {
			abortUnauthorized(c, reasonMissingToken)
			return
		}

		userID, err := jwt.Verify(secret, token)
		if err != nil {
			abortUnauthorized(c, verifyReason(err))
			return
		}

		c.Set(UserIDKey, userID)

		// Backstop: if TraceMiddleware did not run upstream, mint a trace_id
		// so authenticated requests still carry one through the call chain.
		if _, exists := c.Get(TraceIDKey); !exists {
			id := traceIDOrNew(c)
			c.Set(TraceIDKey, id)
			c.Header("X-Trace-Id", id)
		}

		c.Next()
	}
}

// bearerToken extracts the credential portion of a "Bearer <token>" header.
// The boolean is false when the header is absent or not a Bearer scheme.
func bearerToken(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	if !strings.HasPrefix(header, bearerPrefix) {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(header, bearerPrefix))
	if tok == "" {
		return "", false
	}
	return tok, true
}

// verifyReason maps a jwt.Verify failure to the spec-mandated message. The
// jwt package wraps the underlying library error with %w, so errors.Is still
// unwraps to jwtpkg.ErrTokenExpired.
func verifyReason(err error) string {
	if errors.Is(err, jwtpkg.ErrTokenExpired) {
		return reasonExpiredToken
	}
	return reasonInvalidToken
}

// abortUnauthorized writes the unified 401 error body and stops the chain.
func abortUnauthorized(c *gin.Context, message string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, errorBody("UNAUTHORIZED", message))
}

// errorBody is the shared {"error":{"code":..,"message":..}} envelope mandated
// by the api-server spec. It lives here so middleware-issued errors and
// handler-issued errors stay shape-identical; handlers re-declare their own
// copy only when they need BAD_REQUEST etc.
func errorBody(code, message string) gin.H {
	return gin.H{
		"error": gin.H{
			"code":    code,
			"message": message,
		},
	}
}

// UserIDFromCtx returns the authenticated user_id published by AuthMiddleware,
// or an empty string when the request was not authenticated.
func UserIDFromCtx(c *gin.Context) string {
	v, _ := c.Get(UserIDKey)
	id, _ := v.(string)
	return id
}
