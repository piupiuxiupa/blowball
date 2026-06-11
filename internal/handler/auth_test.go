package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/jwt"
	"github.com/lush/blowball/internal/service"
)

const handlerTestSecret = "handler-test-secret"

func init() {
	gin.SetMode(gin.TestMode)
}

// fakeStore mirrors service.fakeUserStore but lives in the handler package so
// we can drive the full Login path through AuthService without spinning up a
// database.
type fakeStore struct{ users map[string]*model.User }

func (f *fakeStore) GetUserByUsername(_ context.Context, username string) (*model.User, error) {
	if u, ok := f.users[username]; ok {
		return u, nil
	}
	return nil, nil
}

func newHandler(t *testing.T, users ...*model.User) (*AuthHandler, *gin.Engine) {
	t.Helper()
	store := &fakeStore{users: map[string]*model.User{}}
	for _, u := range users {
		store.users[u.Username] = u
	}
	svc := service.NewAuthService(store, handlerTestSecret, time.Hour)
	h := NewAuthHandler(svc)
	r := gin.New()
	r.POST("/api/v1/auth/login", h.Login)
	return h, r
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	require.NoError(t, err)
	return string(h)
}

type loginResp struct {
	AccessToken string `json:"access_token"`
	Expire      int64  `json:"expire"`
	TokenType   string `json:"token_type"`
}

type errEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func doPost(t *testing.T, engine *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestLoginHandler_Success(t *testing.T) {
	const (
		username = "alice"
		password = "correct"
		userID   = "uid-alice"
	)
	_, engine := newHandler(t, &model.User{
		UserID:   userID,
		Username: username,
		Password: mustHash(t, password),
		Status:   model.UserStatusActive,
	})

	w := doPost(t, engine, `{"username":"alice","password":"correct"}`)
	require.Equal(t, http.StatusOK, w.Code)

	var resp loginResp
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.NotEmpty(t, resp.AccessToken)
	assert.Equal(t, "Bearer", resp.TokenType)
	assert.Greater(t, resp.Expire, time.Now().Unix())

	// The issued token verifies and carries the right user_id.
	gotID, err := jwt.Verify(handlerTestSecret, resp.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, userID, gotID)
}

func TestLoginHandler_BadRequest(t *testing.T) {
	_, engine := newHandler(t)

	w := doPost(t, engine, `{not valid json`)
	require.Equal(t, http.StatusBadRequest, w.Code)

	var env errEnvelope
	require.NoError(t, json.NewDecoder(w.Body).Decode(&env))
	assert.Equal(t, "BAD_REQUEST", env.Error.Code)
	assert.NotEmpty(t, env.Error.Message)
}

func TestLoginHandler_UserNotFound(t *testing.T) {
	_, engine := newHandler(t)

	w := doPost(t, engine, `{"username":"ghost","password":"whatever"}`)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	var env errEnvelope
	require.NoError(t, json.NewDecoder(w.Body).Decode(&env))
	assert.Equal(t, "UNAUTHORIZED", env.Error.Code)
	assert.Equal(t, "user not found", env.Error.Message)
}

func TestLoginHandler_InvalidCredentials(t *testing.T) {
	_, engine := newHandler(t, &model.User{
		UserID:   "uid-bob",
		Username: "bob",
		Password: mustHash(t, "right"),
		Status:   model.UserStatusActive,
	})

	w := doPost(t, engine, `{"username":"bob","password":"wrong"}`)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	var env errEnvelope
	require.NoError(t, json.NewDecoder(w.Body).Decode(&env))
	assert.Equal(t, "UNAUTHORIZED", env.Error.Code)
	assert.Equal(t, "invalid credentials", env.Error.Message)
}

func TestLoginHandler_DisabledUser(t *testing.T) {
	_, engine := newHandler(t, &model.User{
		UserID:   "uid-carol",
		Username: "carol",
		Password: mustHash(t, "x"),
		Status:   model.UserStatusDisabled,
	})

	w := doPost(t, engine, `{"username":"carol","password":"x"}`)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	var env errEnvelope
	require.NoError(t, json.NewDecoder(w.Body).Decode(&env))
	assert.Equal(t, "user disabled", env.Error.Message)
}

func TestLoginHandler_MissingFields(t *testing.T) {
	// Empty username routes to the not-found path; empty password to invalid
	// credentials. Both must come back as 401, never 500.
	_, engine := newHandler(t, &model.User{
		UserID:   "uid-dave",
		Username: "dave",
		Password: mustHash(t, "x"),
		Status:   model.UserStatusActive,
	})

	w := doPost(t, engine, `{"username":"","password":""}`)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// Valid JSON body that bind succeeds on (both fields are present, just
	// empty strings) still flows through to the service, so this is a 401 not
	// a 400 — the spec reserves 400 for malformed JSON only.
	var env errEnvelope
	require.NoError(t, json.NewDecoder(w.Body).Decode(&env))
	assert.Equal(t, "UNAUTHORIZED", env.Error.Code)
}
