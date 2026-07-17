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

	// SessionUpdateTitle handles PATCH /api/v1/sessions/:session_id. Required.
	SessionUpdateTitle gin.HandlerFunc

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

	// WorkspaceRename handles PUT /api/v1/workspace/files/*path. Required.
	WorkspaceRename gin.HandlerFunc

	// WorkspaceOnlyOfficeConfig handles GET /api/v1/workspace/files/*path/onlyoffice-config.
	// It signs a DocEditor config with the OnlyOffice secret (Bearer auth, like
	// /content). Required; nil panics if hit, so callers must always wire it.
	WorkspaceOnlyOfficeConfig gin.HandlerFunc

	// WorkspaceOnlyOfficeCallback handles POST /api/v1/workspace/onlyoffice-callback.
	// It persists document-save callbacks from the DocumentServer (query-token
	// auth, like token-download). Required.
	WorkspaceOnlyOfficeCallback gin.HandlerFunc

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

// tokenDownloadPath is the special catch-all prefix that selects the
// query-token download handler. gin does not allow a static
// /workspace/files/download/*path route to coexist with /workspace/files/*path,
// so the single catch-all dispatches to WorkspaceTokenDownload when the captured
// path equals this value or starts with "download/".
const tokenDownloadPath = "download"

// onlyOfficeConfigSuffix is the URL suffix that selects the OnlyOffice
// editor-config signing handler over the download handler. Like /content it
// shares the single catch-all route and dispatches on this trailing suffix.
const onlyOfficeConfigSuffix = "/onlyoffice-config"

// onlyOfficeCallbackRoute is the static POST route that receives OnlyOffice
// document-save callbacks. Unlike the catch-all GETs it is a standalone route so
// it can carry the query-token auth middleware without colliding with gin's
// wildcard registration.
const onlyOfficeCallbackRoute = "/workspace/onlyoffice-callback"

// RegisterRoutes wires every route onto r per the api-server spec:
//
//	POST /api/v1/auth/login                       (public)
//	GET  /api/v1/sessions                         (auth)
//	POST /api/v1/sessions                         (auth)
//	PATCH /api/v1/sessions/:session_id            (auth)
//	GET  /api/v1/sessions/:session_id/messages    (auth)
//	POST /api/v1/sessions/:session_id/messages    (auth, SSE)
//	DELETE /api/v1/sessions/:session_id           (auth)
//	GET  /api/v1/workspace/files                  (auth)
//	POST /api/v1/workspace/upload                 (auth)
//	GET  /api/v1/workspace/files/download/*path    (query token auth)
//	GET  /api/v1/workspace/files/*path             (auth, download)
//	GET  /api/v1/workspace/files/*path/content     (auth, text content)
//	GET  /api/v1/workspace/files/*path/onlyoffice-config (auth, signed editor config)
//	PUT  /api/v1/workspace/files/*path             (auth, rename file/dir)
//	DELETE /api/v1/workspace/files/*path          (auth, delete file/dir)
//	POST /api/v1/workspace/onlyoffice-callback     (query token, save callback)
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
	authed.PATCH("/sessions/:session_id", deps.SessionUpdateTitle)
	authed.DELETE("/sessions/:session_id", deps.SessionDelete)

	authed.GET("/workspace/files", deps.WorkspaceList)
	authed.POST("/workspace/upload", deps.WorkspaceUpload)

	// GET workspace files uses a single catch-all. Auth is route-specific:
	// /workspace/files/download/*path uses the query token; everything else uses
	// the Authorization header. This keeps the URL space identical while working
	// around gin's refusal to register a static /download sibling alongside
	// the /*path wildcard.
	v1.GET("/workspace/files/*path", workspaceFileAuthMW(deps), dispatchWorkspaceFile(deps))

	// DELETE shares the same catch-all path under a different method; gin
	// routes per method so this coexists with the GET catch-all above.
	authed.DELETE("/workspace/files/*path", deps.WorkspaceDelete)

	// PUT also shares the catch-all for rename/move operations.
	authed.PUT("/workspace/files/*path", deps.WorkspaceRename)

	// OnlyOffice save callback. It is a standalone POST (not under the catch-all)
	// so it can use the query-token middleware — OnlyOffice posts from the
	// DocumentServer container and cannot send the Bearer header. The middleware
	// aborts on a missing/invalid token; on success it falls through to the
	// handler, which replies with OnlyOffice's {"error": N} convention.
	v1.POST(onlyOfficeCallbackRoute, deps.QueryTokenAuthMW, deps.WorkspaceOnlyOfficeCallback)

	authed.GET("/mcp/tools", deps.MCPTools)
	authed.GET("/skills", deps.SkillsList)
}

// workspaceFileAuthMW selects the appropriate auth middleware for the
// workspace file catch-all. The token-download pseudo-path
// (/workspace/files/download/*path) uses QueryTokenAuthMW; all other paths
// require the Bearer header.
func workspaceFileAuthMW(deps RouteDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimPrefix(c.Param("path"), "/")
		if raw == tokenDownloadPath || strings.HasPrefix(raw, tokenDownloadPath+"/") {
			deps.QueryTokenAuthMW(c)
			return
		}
		deps.AuthMW(c)
	}
}

// dispatchWorkspaceFile returns a gin handler that forwards to
// WorkspaceContent when the captured *path ends with "/content", to
// WorkspaceTokenDownload when the captured path equals "download" or starts
// with "download/", and to WorkspaceDownload otherwise.
func dispatchWorkspaceFile(deps RouteDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimPrefix(c.Param("path"), "/")
		if raw == tokenDownloadPath || strings.HasPrefix(raw, tokenDownloadPath+"/") {
			// Strip the "download/" prefix so TokenDownload reads the file path
			// from c.Param("path") just like the regular download handler.
			filePath := strings.TrimPrefix(raw, tokenDownloadPath)
			filePath = strings.TrimPrefix(filePath, "/")
			c.Params = setPathParam(c.Params, filePath)
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
		if strings.HasSuffix(raw, onlyOfficeConfigSuffix) {
			trimmed := strings.TrimSuffix(raw, onlyOfficeConfigSuffix)
			c.Params = setPathParam(c.Params, trimmed)
			deps.WorkspaceOnlyOfficeConfig(c)
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

// setPathParam returns a copy of params with the "path" key replaced by value.
// It mirrors stripContentSuffix but uses a more general name for the download
// dispatcher, which needs to rewrite the catch-all value from "download/foo" to
// "foo" before invoking TokenDownload.
func setPathParam(params gin.Params, value string) gin.Params {
	out := make(gin.Params, 0, len(params))
	for _, p := range params {
		if p.Key == "path" {
			out = append(out, gin.Param{Key: "path", Value: value})
			continue
		}
		out = append(out, p)
	}
	return out
}
