package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// RouteDeps bundles every handler and middleware dependency RegisterRoutes
// needs. Phase 10's main.go constructs this once and hands it over; tests can
// build a minimal RouteDeps with stub handlers to exercise the wiring.
type RouteDeps struct {
	// AuthMW is the gin middleware that validates the Bearer JWT and publishes
	// user_id on the context. Required; routes in the protected group will not
	// function without it.
	AuthMW gin.HandlerFunc

	// LoginHandler handles POST /api/v1/auth/login (public, outside the auth
	// group). Required.
	Login gin.HandlerFunc

	// SessionList handles GET /api/v1/sessions. Required.
	SessionList gin.HandlerFunc

	// SendMessage handles POST /api/v1/sessions/:session_id/messages (SSE). Required.
	SendMessage gin.HandlerFunc

	// WorkspaceList handles GET /api/v1/workspace/files. Required.
	WorkspaceList gin.HandlerFunc

	// WorkspaceUpload handles POST /api/v1/workspace/upload. Required.
	WorkspaceUpload gin.HandlerFunc

	// WorkspaceDownload handles GET /api/v1/workspace/files/*path. Required.
	WorkspaceDownload gin.HandlerFunc

	// WorkspaceContent handles GET /api/v1/workspace/files/*path/content. Required.
	WorkspaceContent gin.HandlerFunc

	// MCPTools handles GET /api/v1/mcp/tools. Required.
	MCPTools gin.HandlerFunc

	// SkillsList handles GET /api/v1/skills. Required.
	SkillsList gin.HandlerFunc
}

// contentRouteSuffix is the URL suffix that selects the text-content handler
// over the download handler. Because gin's catch-all parameter must be the
// final path segment, both /files/*path and /files/*path/content share a
// single catch-all route and dispatch internally on this suffix.
const contentRouteSuffix = "/content"

// RegisterRoutes wires every route onto r per the api-server spec:
//
//	POST /api/v1/auth/login                       (public)
//	GET  /api/v1/sessions                         (auth)
//	POST /api/v1/sessions/:session_id/messages    (auth, SSE)
//	GET  /api/v1/workspace/files                  (auth)
//	POST /api/v1/workspace/upload                 (auth)
//	GET  /api/v1/workspace/files/*path            (auth, download)
//	GET  /api/v1/workspace/files/*path/content    (auth, text content)
//	GET  /api/v1/mcp/tools                        (auth)
//	GET  /api/v1/skills                           (auth)
//
// The auth group is mounted at /api/v1 and gated by deps.AuthMW; /auth/login
// is registered outside the group.
//
// gin's catch-all parameter must be the final path segment, so the download
// and content endpoints share one catch-all route on /workspace/files/*path
// and dispatch internally on a trailing "/content" suffix.
func RegisterRoutes(r *gin.Engine, deps RouteDeps) {
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	v1 := r.Group("/api/v1")
	v1.POST("/auth/login", deps.Login)

	authed := v1.Group("/")
	authed.Use(deps.AuthMW)

	authed.GET("/sessions", deps.SessionList)
	authed.POST("/sessions/:session_id/messages", deps.SendMessage)

	authed.GET("/workspace/files", deps.WorkspaceList)
	authed.POST("/workspace/upload", deps.WorkspaceUpload)
	// Single catch-all for both download and content; dispatch on the suffix.
	authed.GET("/workspace/files/*path", dispatchWorkspaceFile(deps))

	authed.GET("/mcp/tools", deps.MCPTools)
	authed.GET("/skills", deps.SkillsList)
}

// dispatchWorkspaceFile returns a gin handler that forwards to
// WorkspaceContent when the captured *path ends with "/content", and to
// WorkspaceDownload otherwise. The "/content" suffix is stripped before the
// content handler sees the path so WorkspaceContent reads the actual file
// path the client meant to inspect.
func dispatchWorkspaceFile(deps RouteDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.Param("path")
		if strings.HasSuffix(raw, contentRouteSuffix) {
			trimmed := strings.TrimSuffix(raw, contentRouteSuffix)
			// Re-set the param so WorkspaceContent reads the trimmed value.
			c.Params = stripContentSuffix(c.Params, trimmed)
			deps.WorkspaceContent(c)
			return
		}
		deps.WorkspaceDownload(c)
	}
}

// stripContentSuffix returns a copy of params with the "path" key replaced by
// trimmed. gin.Params is a slice; we replace the entry in place rather than
// reconstructing the slice so any other params are preserved.
func stripContentSuffix(params gin.Params, trimmed string) gin.Params {
	out := make(gin.Params, 0, len(params))
	for _, p := range params {
		if p.Key == "path" {
			out = append(out, gin.Param{Key: "path", Value: trimmed})
			continue
		}
		out = append(out, p)
	}
	return out
}
