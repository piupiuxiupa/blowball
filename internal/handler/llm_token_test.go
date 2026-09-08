package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/service"
)

// fakeLLMTokenStore is the handler-test in-memory service.LLMTokenStore.
type fakeLLMTokenStore struct {
	mu   sync.Mutex
	rows map[string]model.UserLLMCredential
}

func (f *fakeLLMTokenStore) GetUserLLMCredential(_ context.Context, userID string) (*model.UserLLMCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cred, ok := f.rows[userID]
	if !ok {
		return nil, nil
	}
	return &cred, nil
}

func (f *fakeLLMTokenStore) UpsertUserLLMCredential(_ context.Context, cred model.UserLLMCredential) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = make(map[string]model.UserLLMCredential)
	}
	f.rows[cred.UserID] = cred
	return nil
}

func (f *fakeLLMTokenStore) DeleteUserLLMCredential(_ context.Context, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, userID)
	return nil
}

// newLLMTokenRouter builds an engine with the three token routes behind an
// auth middleware that authenticates the given userID (tests exercise the
// handler contract; the JWT verification itself is the middleware's concern).
func newLLMTokenRouter(userID string) (*gin.Engine, *fakeLLMTokenStore) {
	gin.SetMode(gin.TestMode)
	store := &fakeLLMTokenStore{}
	h := NewLLMTokenHandler(service.NewLLMTokenService(store))
	r := gin.New()
	v1 := r.Group("/api/v1")
	authed := v1.Group("/")
	authed.Use(func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	})
	authed.GET("/me/llm-token", h.Get)
	authed.PUT("/me/llm-token", h.Put)
	authed.DELETE("/me/llm-token", h.Delete)
	return r, store
}

func driveLLMToken(method, path, userID, body string) *httptest.ResponseRecorder {
	r, _ := newLLMTokenRouter(userID)
	return driveLLMTokenOn(r, method, path, body)
}

func driveLLMTokenOn(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestLLMTokenHandler_GetUnconfigured(t *testing.T) {
	w := driveLLMToken(http.MethodGet, "/api/v1/me/llm-token", "user-1", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"configured": false}`, w.Body.String())
}

func TestLLMTokenHandler_PutThenGetReturnsMaskedPreviewOnly(t *testing.T) {
	r, _ := newLLMTokenRouter("user-1")
	w := driveLLMTokenOn(r, http.MethodPut, "/api/v1/me/llm-token", `{"api_key": "sk-live-abcd1234"}`)
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"configured": true, "masked_key": "***1234"}`, w.Body.String())
	assert.NotContains(t, w.Body.String(), "sk-live", "the token itself must never appear in a response")

	w = driveLLMTokenOn(r, http.MethodGet, "/api/v1/me/llm-token", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"configured": true, "masked_key": "***1234"}`, w.Body.String())
	assert.NotContains(t, w.Body.String(), "sk-live")
}

func TestLLMTokenHandler_PutValidation(t *testing.T) {
	w := driveLLMToken(http.MethodPut, "/api/v1/me/llm-token", "user-1", `{"api_key": "   "}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")

	w = driveLLMToken(http.MethodPut, "/api/v1/me/llm-token", "user-1", `{"api_key": "`+strings.Repeat("x", 513)+`"}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")

	// Malformed JSON body.
	w = driveLLMToken(http.MethodPut, "/api/v1/me/llm-token", "user-1", `{`)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLLMTokenHandler_DeleteFallsBackToUnconfigured(t *testing.T) {
	r, _ := newLLMTokenRouter("user-1")
	w := driveLLMTokenOn(r, http.MethodPut, "/api/v1/me/llm-token", `{"api_key": "sk-to-remove-9999"}`)
	require.Equal(t, http.StatusOK, w.Code)

	w = driveLLMTokenOn(r, http.MethodDelete, "/api/v1/me/llm-token", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"configured": false}`, w.Body.String())

	w = driveLLMTokenOn(r, http.MethodGet, "/api/v1/me/llm-token", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"configured": false}`, w.Body.String())

	// Deleting an absent token still succeeds.
	w = driveLLMTokenOn(r, http.MethodDelete, "/api/v1/me/llm-token", "")
	require.Equal(t, http.StatusOK, w.Code)
}

func TestLLMTokenHandler_UserIsolation(t *testing.T) {
	// One engine whose auth middleware switches identity per request via the
	// X-Test-User header, so both users share one store.
	gin.SetMode(gin.TestMode)
	store := &fakeLLMTokenStore{}
	h := NewLLMTokenHandler(service.NewLLMTokenService(store))
	r := gin.New()
	v1 := r.Group("/api/v1")
	authed := v1.Group("/")
	authed.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, c.GetHeader("X-Test-User"))
		c.Next()
	})
	authed.GET("/me/llm-token", h.Get)
	authed.PUT("/me/llm-token", h.Put)
	authed.DELETE("/me/llm-token", h.Delete)
	asUser := func(method, body, userID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1/me/llm-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-User", userID)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	w := asUser(http.MethodPut, `{"api_key": "sk-user-a-secret-11"}`, "user-a")
	require.Equal(t, http.StatusOK, w.Code)

	// User B sees only their own (unconfigured) state, never A's token.
	w = asUser(http.MethodGet, "", "user-b")
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"configured": false}`, w.Body.String())

	// A's state is untouched by B's operations.
	w = asUser(http.MethodDelete, "", "user-b")
	require.Equal(t, http.StatusOK, w.Code)
	w = asUser(http.MethodGet, "", "user-a")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"configured":true`)
}

func TestLLMTokenRoutes_RequireAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &fakeLLMTokenStore{}
	h := NewLLMTokenHandler(service.NewLLMTokenService(store))
	r := gin.New()
	v1 := r.Group("/api/v1")
	authed := v1.Group("/")
	authed.Use(middleware.AuthMiddleware("test-secret"))
	authed.GET("/me/llm-token", h.Get)
	authed.PUT("/me/llm-token", h.Put)
	authed.DELETE("/me/llm-token", h.Delete)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/me/llm-token", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code, "%s without a JWT must be rejected", method)
	}
}

func TestLLMTokenRoutes_PartitionOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &fakeLLMTokenStore{}
	h := NewLLMTokenHandler(service.NewLLMTokenService(store))
	deps := RouteDeps{
		LLMTokenGet:    h.Get,
		LLMTokenPut:    h.Put,
		LLMTokenDelete: h.Delete,
	}

	// The agent partition must NOT register the token routes.
	agentEngine := gin.New()
	RegisterAgentRoutes(agentEngine, deps)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/me/llm-token", nil)
		w := httptest.NewRecorder()
		agentEngine.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code, "agent role must not serve %s /me/llm-token", method)
	}

	// The api partition registers them (behind its auth group).
	apiEngine := gin.New()
	deps.AuthMW = func(c *gin.Context) { c.Set(middleware.UserIDKey, "user-1"); c.Next() }
	RegisterAPIRoutes(apiEngine, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/llm-token", nil)
	w := httptest.NewRecorder()
	apiEngine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}
