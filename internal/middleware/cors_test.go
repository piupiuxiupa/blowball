package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// TestCORS_PatchPreflightIncludesPatch verifies that an OPTIONS preflight for
// PATCH receives Access-Control-Allow-Methods containing PATCH. This guards
// the session-title-update endpoint against browser/Swagger UI CORS blocks.
func TestCORS_PatchPreflightIncludesPatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CORS())
	r.PATCH("/api/v1/sessions/:session_id", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/sessions/s-1", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", http.MethodPatch)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	allowed := w.Header().Get("Access-Control-Allow-Methods")
	assert.Contains(t, allowed, http.MethodPatch, "PATCH must be advertised in Allow-Methods")
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
}

// TestCORS_PatchActualRequest adds the CORS headers for a real PATCH request.
func TestCORS_PatchActualRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CORS())
	r.PATCH("/api/v1/sessions/:session_id", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/sessions/s-1", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Methods"), http.MethodPatch)
}
