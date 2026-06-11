// Package handler adapts the service layer to gin HTTP handlers. Each handler
// stays thin: parse the request, delegate to a service, map the service error
// onto the unified error response shape.
package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lush/blowball/internal/service"
)

// AuthHandler owns the /api/v1/auth/* routes.
type AuthHandler struct {
	svc *service.AuthService
}

// NewAuthHandler wires the handler with its backing service.
func NewAuthHandler(svc *service.AuthService) *AuthHandler {
	return &AuthHandler{svc: svc}
}

// loginRequest is the JSON body for POST /api/v1/auth/login.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// loginResponse is the JSON body returned on a successful login.
type loginResponse struct {
	AccessToken string `json:"access_token"`
	Expire      int64  `json:"expire"`
	TokenType   string `json:"token_type"`
}

// Login handles POST /api/v1/auth/login. On success it returns 200 with the
// issued JWT; on sentinel service errors it returns 401 with a spec-mandated
// message; on JSON parse failure it returns 400 BAD_REQUEST.
func (h *AuthHandler) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", err.Error()))
		return
	}

	token, expireAt, err := h.svc.Login(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		writeAuthError(c, err)
		return
	}

	c.JSON(http.StatusOK, loginResponse{
		AccessToken: token,
		Expire:      expireAt.Unix(),
		TokenType:   "Bearer",
	})
}

// writeAuthError maps a service.Login error onto the unified 401 response. Any
// error that is not one of the known sentinels is collapsed to a generic
// "invalid credentials" so internal failure details never leak to the client.
func writeAuthError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrUserNotFound):
		c.JSON(http.StatusUnauthorized, errorBody("UNAUTHORIZED", "user not found"))
	case errors.Is(err, service.ErrInvalidCredentials):
		c.JSON(http.StatusUnauthorized, errorBody("UNAUTHORIZED", "invalid credentials"))
	case errors.Is(err, service.ErrUserDisabled):
		c.JSON(http.StatusUnauthorized, errorBody("UNAUTHORIZED", "user disabled"))
	default:
		c.JSON(http.StatusUnauthorized, errorBody("UNAUTHORIZED", "invalid credentials"))
	}
}

// errorBody is the {"error":{"code":..,"message":..}} envelope mandated by the
// api-server spec.
func errorBody(code, message string) gin.H {
	return gin.H{
		"error": gin.H{
			"code":    code,
			"message": message,
		},
	}
}
