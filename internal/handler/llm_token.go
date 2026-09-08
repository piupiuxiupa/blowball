package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/service"
)

// LLMTokenHandler owns the per-user LLM token management endpoints
// (user-llm-token): GET/PUT/DELETE /api/v1/me/llm-token. The routes sit in
// the authenticated api partition; the user identity comes exclusively from
// the verified JWT (never from the body), so cross-user access is impossible
// by construction. No response ever carries the token itself.
type LLMTokenHandler struct {
	svc *service.LLMTokenService
}

// NewLLMTokenHandler wires the handler with its service.
func NewLLMTokenHandler(svc *service.LLMTokenService) *LLMTokenHandler {
	return &LLMTokenHandler{svc: svc}
}

// llmTokenStatus is the shared response shape: configured plus a masked
// preview that is omitted entirely when no token is stored.
type llmTokenStatus struct {
	Configured bool   `json:"configured"`
	MaskedKey  string `json:"masked_key,omitempty"`
}

// Get handles GET /api/v1/me/llm-token.
func (h *LLMTokenHandler) Get(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	configured, masked, err := h.svc.Status(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "llm token lookup failed"))
		return
	}
	c.JSON(http.StatusOK, llmTokenStatus{Configured: configured, MaskedKey: masked})
}

// Put handles PUT /api/v1/me/llm-token. The body is {"api_key": "..."}; the
// token is trimmed and validated (non-empty, ≤512 chars) then stored as an
// idempotent overwrite. The stored token is never echoed — only its mask.
func (h *LLMTokenHandler) Put(c *gin.Context) {
	var req struct {
		APIKey string `json:"api_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", err.Error()))
		return
	}
	userID := middleware.UserIDFromCtx(c)
	masked, err := h.svc.Save(c.Request.Context(), userID, req.APIKey)
	if err != nil {
		if errors.Is(err, service.ErrInvalidLLMToken) {
			c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", err.Error()))
			return
		}
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "llm token save failed"))
		return
	}
	c.JSON(http.StatusOK, llmTokenStatus{Configured: true, MaskedKey: masked})
}

// Delete handles DELETE /api/v1/me/llm-token. Subsequent LLM calls for this
// user fall back to the deployment-global openai.api_key. Deleting an absent
// token is a success.
func (h *LLMTokenHandler) Delete(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	if err := h.svc.Delete(c.Request.Context(), userID); err != nil {
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "llm token delete failed"))
		return
	}
	c.JSON(http.StatusOK, llmTokenStatus{Configured: false})
}
