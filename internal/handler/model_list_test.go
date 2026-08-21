package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lush/blowball/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// driveModelsList registers GET /api/v1/models behind a no-op auth middleware
// (auth is enforced by the route group, not the handler) and drives it.
func driveModelsList(t *testing.T, h *ModelListHandler) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	v1 := r.Group("/api/v1")
	authed := v1.Group("/")
	authed.Use(func(c *gin.Context) { c.Next() })
	authed.GET("/models", h.List)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestModelListHandler_CatalogEcho verifies the catalog echo: every entry
// (name / max_context_tokens / max_completion_tokens / thinking), the default
// name, and the deployment default reasoning effort (model-effort-v2's
// additive field the frontend uses to preselect the effort picker). The
// per-entry length_continue block must NOT leak into the response
// (per-model-completion-budget D5: an operator-side switch, not a selection
// axis).
func TestModelListHandler_CatalogEcho(t *testing.T) {
	h := NewModelListHandler([]config.ModelCatalogEntry{
		{Name: "gpt-5", MaxContextTokens: 400000, MaxCompletionTokens: 16384, Thinking: true,
			LengthContinue: config.LengthContinueConfig{ExpandStep: 8192, MaxRetries: 2}},
		{Name: "glm-4.7", MaxContextTokens: 200000, MaxCompletionTokens: 8192, Thinking: false},
	}, "gpt-5", "high")

	w := driveModelsList(t, h)
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Models               []modelEntry `json:"models"`
		Default              string       `json:"default"`
		DefaultReasoningEfft string       `json:"default_reasoning_effort"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Models, 2)
	assert.Equal(t, modelEntry{Name: "gpt-5", MaxContextTokens: 400000, MaxCompletionTokens: 16384, Thinking: true}, body.Models[0])
	assert.Equal(t, modelEntry{Name: "glm-4.7", MaxContextTokens: 200000, MaxCompletionTokens: 8192, Thinking: false}, body.Models[1])
	assert.Equal(t, "gpt-5", body.Default)
	assert.Equal(t, "high", body.DefaultReasoningEfft)
	assert.NotContains(t, w.Body.String(), "length_continue", "the per-entry continuation switch is operator-only and must not be exposed")
	assert.NotContains(t, w.Body.String(), "expand_step")
}

// TestModelListHandler_DefaultEffortNone: the normalized unset default
// surfaces as the literal "none".
func TestModelListHandler_DefaultEffortNone(t *testing.T) {
	h := NewModelListHandler([]config.ModelCatalogEntry{
		{Name: "gpt-4o-mini", MaxContextTokens: 128000, Thinking: false},
	}, "gpt-4o-mini", "none")

	w := driveModelsList(t, h)
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Models               []modelEntry `json:"models"`
		Default              string       `json:"default"`
		DefaultReasoningEfft string       `json:"default_reasoning_effort"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Models, 1)
	assert.Equal(t, "gpt-4o-mini", body.Default)
	assert.Equal(t, "none", body.DefaultReasoningEfft)
}

// TestModelListHandler_UnauthenticatedRejected verifies the endpoint sits
// behind the auth middleware: with a rejecting middleware the handler never
// runs and the client sees 401 (the spec's "未认证被拒" scenario — the route
// registration carries AuthMW, this drives the same wiring).
func TestModelListHandler_UnauthenticatedRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	v1 := r.Group("/api/v1")
	authed := v1.Group("/")
	authed.Use(func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "UNAUTHORIZED"}})
	})
	h := NewModelListHandler(nil, "", "none")
	authed.GET("/models", h.List)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
