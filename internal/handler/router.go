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

	// QueryTokenAuthMW validates the JWT from the URL query parameter "token".
	// It gates the token-download endpoint so the rest of the API remains
	// header-authenticated only.
	QueryTokenAuthMW gin.HandlerFunc

	// WorkspaceTokenDownload serves GET /api/v1/workspace/files/download.
	// Authenticated via QueryTokenAuthMW instead of AuthMW.
	WorkspaceTokenDownload gin.HandlerFunc

	// Login handles POST /api/v1/auth/login (public, outside the auth group).
	// Required.
	Login gin.HandlerFunc

	// SessionList handles GET /api/v1/sessions. Required.
	SessionList gin.HandlerFunc

	// SessionCreate handles POST /api/v1/sessions. Required.
	SessionCreate gin.HandlerFunc

	// SessionMessages handles GET /api/v1/sessions/:session_id/messages. Required.
	SessionMessages gin.HandlerFunc

	// SendMessage handles POST /api/v1/sessions/:session_id/messages (SSE). Required.
	SendMessage gin.HandlerFunc

	// SessionDelete handles DELETE /api/v1/sessions/:session_id. Required.
	SessionDelete gin.HandlerFunc

	// WorkspaceList handles GET /api/v1/workspace/files. Required.
	WorkspaceList gin.HandlerFunc

	// WorkspaceUpload handles POST /api/v1/workspace/upload. Required.
	WorkspaceUpload gin.HandlerFunc

	// WorkspaceDownload handles GET /api/v1/workspace/files/*path. Required.
	WorkspaceDownload gin.HandlerFunc

	// WorkspaceContent handles GET /api/v1/workspace/files/*path/content. Required.
	WorkspaceContent gin.HandlerFunc

	// WorkspaceDelete handles DELETE /api/v1/workspace/files/*path. Required.
	WorkspaceDelete gin.HandlerFunc

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

// tokenDownloadPath is the special catch-all value that selects the
// query-token download handler. gin does not allow a static
// /workspace/files/download route to coexist with /workspace/files/*path, so
// the single catch-all dispatches to WorkspaceTokenDownload when the captured
// path is exactly this value.
const tokenDownloadPath = "download"

// RegisterRoutes wires every route onto r per the api-server spec:
//
//	POST /api/v1/auth/login                       (public)
//	GET  /api/v1/sessions                         (auth)
//	POST /api/v1/sessions                         (auth)
//	GET  /api/v1/sessions/:session_id/messages    (auth)
//	POST /api/v1/sessions/:session_id/messages    (auth, SSE)
//	DELETE /api/v1/sessions/:session_id           (auth)
//	GET  /api/v1/workspace/files                  (auth)
//	POST /api/v1/workspace/upload                 (auth)
//	GET  /api/v1/workspace/files/download         (query token auth)
//	GET  /api/v1/workspace/files/*path            (auth, download)
//	GET  /api/v1/workspace/files/*path/content    (auth, text content)
//	DELETE /api/v1/workspace/files/*path          (auth, delete file/dir)
//	GET  /api/v1/mcp/tools                        (auth)
//	GET  /api/v1/skills                           (auth)
//
// The auth group is mounted at /api/v1 and gated by deps.AuthMW; /auth/login
// is registered outside the group.
//
// gin's catch-all parameter must be the final path segment, so the download
// and content endpoints share one catch-all route on /workspace/files/*path
// and dispatch internally on a trailing "/content" suffix. The token-download
// endpoint is reached through the same catch-all with path == "download";
// a separate static route cannot be registered because gin rejects a static
// segment and a wildcard at the same tree node.
func RegisterRoutes(r *gin.Engine, deps RouteDeps) {
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	v1 := r.Group("/api/v1")
	v1.POST("/auth/login", deps.Login)

	authed := v1.Group("/")
	authed.Use(deps.AuthMW)

	authed.GET("/sessions", deps.SessionList)
	authed.POST("/sessions", deps.SessionCreate)
	authed.GET("/sessions/:session_id/messages", deps.SessionMessages)
	authed.POST("/sessions/:session_id/messages", deps.SendMessage)
	authed.DELETE("/sessions/:session_id", deps.SessionDelete)

	authed.GET("/workspace/files", deps.WorkspaceList)
	authed.POST("/workspace/upload", deps.WorkspaceUpload)

	// GET workspace files uses a single catch-all. Auth is route-specific:
	// /workspace/files/download uses the query token; everything else uses the
	// Authorization header. This keeps the URL space identical while working
	// around gin's refusal to register a static /download sibling alongside
	// the /*path wildcard.
	v1.GET("/workspace/files/*path", workspaceFileAuthMW(deps), dispatchWorkspaceFile(deps))

	// DELETE shares the same catch-all path under a different method; gin
	// routes per method so this coexists with the GET catch-all above.
	authed.DELETE("/workspace/files/*path", deps.WorkspaceDelete)

	authed.GET("/mcp/tools", deps.MCPTools)
	authed.GET("/skills", deps.SkillsList)
}

// workspaceFileAuthMW selects the appropriate auth middleware for the
// workspace file catch-all. The token-download pseudo-path uses
// QueryTokenAuthMW; all other paths require the Bearer header.
func workspaceFileAuthMW(deps RouteDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if strings.TrimPrefix(c.Param("path"), "/") == tokenDownloadPath {
			deps.QueryTokenAuthMW(c)
			return
		}
		deps.AuthMW(c)
	}
}

// dispatchWorkspaceFile returns a gin handler that forwards to
// WorkspaceContent when the captured *path ends with "/content", to
// WorkspaceTokenDownload when the captured path is "download", and to
// WorkspaceDownload otherwise.
func dispatchWorkspaceFile(deps RouteDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimPrefix(c.Param("path"), "/")
		if raw == tokenDownloadPath {
			deps.WorkspaceTokenDownload(c)
			return
		}
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
