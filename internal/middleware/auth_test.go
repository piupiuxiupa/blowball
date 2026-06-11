package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	jwtpkg "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/pkg/jwt"
	"github.com/lush/blowball/internal/pkg/trace"
)

const mwTestSecret = "mw-test-secret"

func init() {
	gin.SetMode(gin.TestMode)
}

// newEngineWithAuth builds a gin engine whose only handler runs behind
// AuthMiddleware; it returns whatever user_id the middleware published, so the
// tests can assert propagation.
func newEngineWithAuth(t *testing.T) *gin.Engine {
	t.Helper()
	r := gin.New()
	r.Use(TraceMiddleware())
	r.GET("/secure", AuthMiddleware(mwTestSecret), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"user_id": UserIDFromCtx(c)})
	})
	return r
}

// doGet fires a GET at engine with the supplied Authorization header.
func doGet(t *testing.T, engine *gin.Engine, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/secure", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// errBody is a tiny helper to decode the unified error envelope.
type errBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) errBody {
	t.Helper()
	var b errBody
	require.NoError(t, json.NewDecoder(w.Body).Decode(&b))
	return b
}

func sign(t *testing.T, userID string, expire time.Duration) string {
	t.Helper()
	tok, err := jwt.Sign(mwTestSecret, userID, expire)
	require.NoError(t, err)
	return tok
}

func TestAuthMiddleware_ValidToken(t *testing.T) {
	const userID = "user-123"
	engine := newEngineWithAuth(t)

	w := doGet(t, engine, "Bearer "+sign(t, userID, time.Hour))
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		UserID string `json:"user_id"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, userID, resp.UserID)
}

func TestAuthMiddleware_MissingToken(t *testing.T) {
	engine := newEngineWithAuth(t)

	w := doGet(t, engine, "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "missing token", decodeErr(t, w).Error.Message)

	// A non-Bearer header should produce the same outcome as a missing one.
	w = doGet(t, engine, "Basic xyz")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "missing token", decodeErr(t, w).Error.Message)
}

func TestAuthMiddleware_ExpiredToken(t *testing.T) {
	engine := newEngineWithAuth(t)

	// Sign a token whose expiry already passed.
	expired := sign(t, "user-x", -1*time.Minute)

	w := doGet(t, engine, "Bearer "+expired)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "token expired", decodeErr(t, w).Error.Message)
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	engine := newEngineWithAuth(t)

	// Each case carries a non-empty Bearer credential that the middleware
	// forwards to jwt.Verify; all of them must come back as "invalid token".
	// Empty/non-Bearer headers are exercised separately under the missing-token
	// scenario, since the spec mandates "missing token" for those.
	cases := []string{
		"Bearer not-a-real-token",
		"Bearer eyJhbGciOiJIUzI1NiJ9.bogus.signature",
		"Bearer a.b.c",
	}
	for _, tok := range cases {
		w := doGet(t, engine, tok)
		require.Equalf(t, http.StatusUnauthorized, w.Code, "token=%q", tok)
		assert.Equalf(t, "invalid token", decodeErr(t, w).Error.Message, "token=%q", tok)
	}

	// A token signed with the wrong secret must also be invalid.
	other, err := jwt.Sign("different-secret", "user-y", time.Hour)
	require.NoError(t, err)
	w := doGet(t, engine, "Bearer "+other)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "invalid token", decodeErr(t, w).Error.Message)
}

// TestVerifyReason_Direct pins the jwt.Verify -> reason mapping without going
// through the gin stack, so a future refactor of the middleware's error
// handling surfaces here loudly.
func TestVerifyReason_Direct(t *testing.T) {
	expired, err := jwt.Sign(mwTestSecret, "u", -time.Minute)
	require.NoError(t, err)
	_, verifyErr := jwt.Verify(mwTestSecret, expired)
	require.Error(t, verifyErr)
	assert.ErrorIs(t, verifyErr, jwtpkg.ErrTokenExpired, "wrapped error must unwrap to jwt.ErrTokenExpired")
	assert.Equal(t, "token expired", verifyReason(verifyErr))

	_, verifyErr = jwt.Verify(mwTestSecret, "garbage")
	assert.Equal(t, "invalid token", verifyReason(verifyErr))
}

func TestTraceMiddleware_SetsTraceID(t *testing.T) {
	r := gin.New()
	r.Use(TraceMiddleware())
	var (
		gotGinID string
		gotCtxID string
	)
	r.GET("/t", func(c *gin.Context) {
		gotGinID = TraceIDFromCtx(c)
		gotCtxID = trace.FromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	// gin.Context key is set and non-empty.
	assert.NotEmpty(t, gotGinID)

	// Header is echoed.
	assert.Equal(t, gotGinID, w.Header().Get("X-Trace-Id"))

	// The standard context.Context also carries the same id, so services
	// reached via c.Request.Context() recover it through trace.FromContext.
	assert.Equal(t, gotGinID, gotCtxID)

	// Each request gets a fresh id.
	firstID := gotGinID
	req2 := httptest.NewRequest(http.MethodGet, "/t", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)
	assert.NotEqual(t, firstID, w2.Header().Get("X-Trace-Id"))
}
