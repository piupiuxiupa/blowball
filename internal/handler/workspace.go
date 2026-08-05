package handler

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/pkg/jwt"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/store/fs"
	"github.com/lush/blowball/internal/tool/xizhi"
)

// WorkspaceHandler owns the /api/v1/workspace/* routes: file listing, upload,
// download, and text-content retrieval. All operations are scoped to the
// authenticated user's workspace directory and validated via
// xizhi.ValidatePathAllowReserved so path traversal and symlink escapes are
// rejected with 403. Unlike the agent's xizhi_* tools, the REST API does NOT
// reserve the .blowball/ namespace, so the user can browse and manage their own
// application state (e.g. per-user skills under .blowball/skills/) directly;
// the agent remains blocked via xizhi.ValidatePath.
type WorkspaceHandler struct {
	fsSvc          *fs.Store
	maxUploadBytes int64
	oo             OnlyOfficeSettings
}

// OnlyOfficeSettings is the server-side OnlyOffice integration config injected
// into WorkspaceHandler. Secret signs the editor config (HS256); ServerURL is
// the browser-facing api.js origin and the host allowlist base for callback
// result URLs; InternalBackend is the container-reachable backend origin used to
// build document.url and callbackUrl. VersionServiceURL is the base URL of the
// external office-vers service that the version-view config endpoint points
// document.url at. When Secret is empty the editor-config and callback endpoints
// return 503 rather than signing an unverifiable config; the version-view
// endpoint additionally requires VersionServiceURL.
type OnlyOfficeSettings struct {
	Secret            string
	ServerURL         string
	InternalBackend   string
	VersionServiceURL string
}

// configured reports whether OnlyOffice editing is enabled. The editor-config
// endpoint cannot sign a valid config without a secret, so an empty secret
// disables both endpoints (503).
func (o OnlyOfficeSettings) configured() bool {
	return strings.TrimSpace(o.Secret) != ""
}

// versionConfigured reports whether the historical-version view config endpoint
// is enabled. It needs BOTH the signing secret and the office-vers service base
// URL, because document.url points at office-vers (not the backend). Either
// missing → 503.
func (o OnlyOfficeSettings) versionConfigured() bool {
	return strings.TrimSpace(o.Secret) != "" && strings.TrimSpace(o.VersionServiceURL) != ""
}

// NewWorkspaceHandler wires the handler. maxUploadBytes is the per-file upload
// cap; uploads larger than this are rejected with 413 before they reach disk.
// A non-positive value disables the cap (not recommended for production). oo is
// the OnlyOffice integration config; pass an empty OnlyOfficeSettings to disable
// office editing (the editor endpoints then return 503).
func NewWorkspaceHandler(fsSvc *fs.Store, maxUploadBytes int64, oo OnlyOfficeSettings) *WorkspaceHandler {
	return &WorkspaceHandler{fsSvc: fsSvc, maxUploadBytes: maxUploadBytes, oo: oo}
}

// fileEntry is one element of the GET /api/v1/workspace/files response array.
type fileEntry struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	UpdateTime string `json:"update_time"`
}

// List handles GET /api/v1/workspace/files[?path=<sub>][&include_hidden=<bool>].
// Returns 200 with a (possibly empty) array of file/dir entries sorted by name.
// Hidden entries (names beginning with ".") are excluded unless include_hidden
// is true. A path that resolves outside the workspace is rejected with 403.
func (h *WorkspaceHandler) List(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	wsRoot := h.fsSvc.UserWorkspace(userID)
	rel := c.Query("path")
	includeHidden := parseBoolQuery(c.Query("include_hidden"))

	target, err := resolveListTarget(wsRoot, rel)
	if err != nil {
		writeForbidden(c, "path outside workspace")
		return
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusOK, gin.H{"files": []fileEntry{}})
			return
		}
		logWS(ctx, "workspace.list", target, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "list workspace failed"))
		return
	}

	out := make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !includeHidden && isHiddenName(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, fileEntry{
			Name:       name,
			Type:       fileType(e),
			Size:       info.Size(),
			UpdateTime: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			// Directories first, then files.
			return out[i].Type == "dir"
		}
		return out[i].Name < out[j].Name
	})

	c.JSON(http.StatusOK, gin.H{"files": out})
	_ = ctx
}

// Upload handles POST /api/v1/workspace/upload. Expects multipart form with a
// "file" field and a "path" field naming the destination subdirectory (empty
// or absent means root). Returns 200 with the absolute path and size on
// success. Errors: 413 too large, 403 path outside workspace, 400 bad form.
func (h *WorkspaceHandler) Upload(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	if h.maxUploadBytes > 0 {
		// Cap the request body BEFORE multipart parsing so an oversized upload
		// is rejected as it arrives rather than after being buffered in full.
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxUploadBytes)
	}

	if err := c.Request.ParseMultipartForm(h.maxUploadBytesMemory()); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, errorBody("FILE_TOO_LARGE", "file too large"))
			return
		}
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", err.Error()))
		return
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "file field is required"))
		return
	}
	if h.maxUploadBytes > 0 && fileHeader.Size > h.maxUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, errorBody("FILE_TOO_LARGE", "file too large"))
		return
	}

	relDir := strings.TrimSpace(c.PostForm("path"))
	wsRoot := h.fsSvc.UserWorkspace(userID)

	dstDir, err := resolveUploadDir(wsRoot, relDir)
	if err != nil {
		writeForbidden(c, "path outside workspace")
		return
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		logWS(ctx, "workspace.upload.mkdir", dstDir, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "create destination dir failed"))
		return
	}

	dstPath := filepath.Join(dstDir, filepath.Base(fileHeader.Filename))
	if err := c.SaveUploadedFile(fileHeader, dstPath); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, errorBody("FILE_TOO_LARGE", "file too large"))
			return
		}
		logWS(ctx, "workspace.upload.save", dstPath, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "save uploaded file failed"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"path": relPath(wsRoot, dstPath),
		"size": fileHeader.Size,
	})
}

// Download handles GET /api/v1/workspace/files/*path. Serves the file with the
// content-type implied by its extension. Non-existent file -> 404; path outside
// workspace -> 403.
func (h *WorkspaceHandler) Download(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	wsRoot := h.fsSvc.UserWorkspace(userID)
	rel := c.Param("path")

	abs, err := xizhi.ValidatePathAllowReserved(wsRoot, strings.TrimPrefix(rel, "/"))
	if err != nil {
		writeForbidden(c, "path outside workspace")
		return
	}

	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "file not found"))
			return
		}
		logWS(ctx, "workspace.download.stat", abs, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "stat file failed"))
		return
	}
	if info.IsDir() {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "path is a directory"))
		return
	}

	c.File(abs)
}

// TokenDownload handles GET /api/v1/workspace/files/download/{path}?token=<jwt>[&inline=1].
// It authenticates via the URL token query parameter and serves the requested
// file with Content-Disposition. This endpoint exists so browser-native
// elements (<a download>, <img>, PDF.js) can access workspace files without
// custom Authorization headers.
func (h *WorkspaceHandler) TokenDownload(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	rel := strings.TrimSpace(c.Param("path"))
	if rel == "" {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "path is required"))
		return
	}

	wsRoot := h.fsSvc.UserWorkspace(userID)
	abs, err := xizhi.ValidatePathAllowReserved(wsRoot, rel)
	if err != nil {
		writeForbidden(c, "path outside workspace")
		return
	}

	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "file not found"))
			return
		}
		logWS(ctx, "workspace.token_download.stat", abs, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "stat file failed"))
		return
	}
	if info.IsDir() {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "path is a directory"))
		return
	}

	inline := c.Query("inline") == "1"
	name := filepath.Base(abs)

	c.Header("Content-Disposition", contentDisposition(name, inline))
	c.Header("Cache-Control", "private, no-store")
	c.Header("Referrer-Policy", "no-referrer")

	c.File(abs)
}

// contentDisposition builds a Content-Disposition header with both an ASCII
// filename fallback and an RFC 5987 filename* value for Unicode names.
func contentDisposition(name string, inline bool) string {
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	return disp + `; filename="` + asciiFilenameFallback(name) + `"; filename*=utf-8''` + rfc5987Encode(name)
}

// asciiFilenameFallback returns an ASCII-only fallback for the filename=
// parameter. Non-ASCII characters are replaced with underscores so legacy
// clients receive a renderable name.
func asciiFilenameFallback(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r >= 32 && r <= 126 {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// rfc5987Encode percent-encodes name for the filename*= parameter using
// UTF-8. Every byte that is not an unreserved URI character is encoded so
// non-ASCII filenames round-trip correctly through browsers.
func rfc5987Encode(name string) string {
	return url.PathEscape(name)
}

// Content handles GET /api/v1/workspace/files/*path/content. Returns the file's
// text content as JSON. Binary files (detected by sniffing the first 512 bytes)
// are rejected with 400 BINARY_FILE so the client knows to use the download
// endpoint instead. Path outside workspace -> 403; missing -> 404.
func (h *WorkspaceHandler) Content(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	wsRoot := h.fsSvc.UserWorkspace(userID)
	rel := c.Param("path")

	abs, err := xizhi.ValidatePathAllowReserved(wsRoot, strings.TrimPrefix(rel, "/"))
	if err != nil {
		writeForbidden(c, "path outside workspace")
		return
	}

	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "file not found"))
			return
		}
		logWS(ctx, "workspace.content.stat", abs, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "stat file failed"))
		return
	}
	if info.IsDir() {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "path is a directory"))
		return
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		logWS(ctx, "workspace.content.read", abs, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "read file failed"))
		return
	}

	if isBinary(data) {
		c.JSON(http.StatusBadRequest, errorBody("BINARY_FILE", "binary file, use download endpoint"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"path":    relPath(wsRoot, abs),
		"content": string(data),
		"size":    len(data),
	})
}

// writeContentRequest is the JSON body for PUT /api/v1/workspace/files/*path/content.
type writeContentRequest struct {
	Content string `json:"content"`
}

// WriteContent handles PUT /api/v1/workspace/files/*path/content. It is the
// symmetric write counterpart to GET .../content: an atomic, create-or-replace
// (HTTP PUT semantics, matching xizhi_write_file) text-content write. A missing
// target file is created (parents auto-created via MkdirAll); an existing file is
// fully replaced through a temp-file + os.Rename so a crash mid-write never
// truncates the destination. The body is capped at maxUploadBytes, and only text
// is accepted: a NUL byte in content is rejected with 400 BINARY_FILE (symmetric
// with the read side, which refuses to return binary). Binary/large files keep
// using POST .../upload.
//
// Errors: 400 BAD_REQUEST (malformed body / target is an existing directory) or
// 400 BINARY_FILE (NUL byte); 403 path outside workspace; 413 FILE_TOO_LARGE;
// 500 INTERNAL on unexpected write failure; 401 missing/invalid JWT (AuthMW).
func (h *WorkspaceHandler) WriteContent(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	if h.maxUploadBytes > 0 {
		// Cap the request body BEFORE JSON decoding so an oversized payload is
		// rejected as it arrives rather than after being buffered in full.
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxUploadBytes)
	}

	var req writeContentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, errorBody("FILE_TOO_LARGE", "file too large"))
			return
		}
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", err.Error()))
		return
	}

	wsRoot := h.fsSvc.UserWorkspace(userID)
	rel := strings.TrimPrefix(c.Param("path"), "/")

	abs, err := xizhi.ValidatePathAllowReserved(wsRoot, rel)
	if err != nil {
		writeForbidden(c, "path outside workspace")
		return
	}

	// Reject a directory target before touching the filesystem: writing "into"
	// a directory is not a text-content write.
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "path is a directory"))
		return
	}

	// Text-only: a NUL byte means binary content the read side could not serve
	// back, so refuse it before staging a temp file.
	if isBinary([]byte(req.Content)) {
		c.JSON(http.StatusBadRequest, errorBody("BINARY_FILE", "binary content, use upload endpoint"))
		return
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		logWS(ctx, "workspace.writecontent.mkdir", filepath.Dir(abs), err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "create destination dir failed"))
		return
	}

	if _, err := atomicWriteFile(abs, []byte(req.Content), h.maxUploadBytes); err != nil {
		if errors.Is(err, errWriteTooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, errorBody("FILE_TOO_LARGE", "file too large"))
			return
		}
		logWS(ctx, "workspace.writecontent.persist", abs, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "write file failed"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"path": relPath(wsRoot, abs),
		"size": len(req.Content),
	})
}

// createNodeRequest is the JSON body for POST /api/v1/workspace/files/*path. The
// body carries only the node kind; the target path comes from the URL catch-all
// (like Rename, whose params live in the body).
type createNodeRequest struct {
	Type string `json:"type"`
}

// createNodeResponse is the JSON body returned by POST /api/v1/workspace/files/*path.
type createNodeResponse struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

// Create handles POST /api/v1/workspace/files/*path. It is a strict create of an
// empty file or directory selected by the body {"type": "file" | "directory"}.
// "Strict" means a target leaf that already exists — file OR directory — is
// rejected with 409 ALREADY_EXISTS and left untouched, with no check-then-create
// window: file creation uses OpenFile(O_CREATE|O_EXCL) and directory creation
// uses os.Mkdir, both of which surface EEXIST on an existing leaf (so two
// concurrent creates of the same path resolve as one 200 and one 409). Missing
// parent directories are auto-created (MkdirAll on the parent), so a nested path
// like a/b/c is established in one call; the strict guarantee applies only to the
// leaf itself. This is distinct from PUT .../content, which is a create-or-replace
// for text content — Create produces empty nodes only.
//
// Creating the workspace root itself (an empty/"/" path) is rejected with 400
// BAD_REQUEST; a missing or invalid type returns 400. Errors: 400 BAD_REQUEST,
// 403 path outside workspace, 409 ALREADY_EXISTS, 500 INTERNAL; 401 missing JWT
// (AuthMW).
func (h *WorkspaceHandler) Create(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	wsRoot := h.fsSvc.UserWorkspace(userID)
	rel := strings.TrimPrefix(c.Param("path"), "/")

	// "Creating" the workspace root is meaningless — it already exists and would
	// surface a confusing EEXIST/EISDIR. Reject it up front. This must precede
	// ValidatePathAllowReserved, which treats an empty path as an escape error
	// (and so would otherwise map the root-create case to 403 instead of 400).
	if rel == "" {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "path is required"))
		return
	}
	abs, err := xizhi.ValidatePathAllowReserved(wsRoot, rel)
	if err != nil {
		writeForbidden(c, "path outside workspace")
		return
	}

	var req createNodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", err.Error()))
		return
	}
	if req.Type != "file" && req.Type != "directory" {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", `type must be "file" or "directory"`))
		return
	}

	// Auto-create missing parent directories (leaf-strict + auto-parents),
	// consistent with WriteContent/Upload. MkdirAll on the parent is idempotent
	// and never touches the leaf, so it cannot weaken the strict guarantee below.
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		logWS(ctx, "workspace.create.mkdir_parent", filepath.Dir(abs), err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "create destination dir failed"))
		return
	}

	if req.Type == "file" {
		// O_EXCL fuses "does not exist" + "create" into one atomic step: an
		// existing leaf surfaces EEXIST (mapped to 409) with no TOCTOU window.
		f, ferr := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if ferr != nil {
			if errors.Is(ferr, os.ErrExist) {
				c.JSON(http.StatusConflict, errorBody("ALREADY_EXISTS", "node already exists"))
				return
			}
			logWS(ctx, "workspace.create.file", abs, ferr)
			c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "create file failed"))
			return
		}
		_ = f.Close()
	} else {
		// os.Mkdir fails with EEXIST when the leaf already exists (file or dir),
		// giving directories the same strict-create contract as the file branch.
		if err := os.Mkdir(abs, 0o755); err != nil {
			if errors.Is(err, os.ErrExist) {
				c.JSON(http.StatusConflict, errorBody("ALREADY_EXISTS", "node already exists"))
				return
			}
			logWS(ctx, "workspace.create.directory", abs, err)
			c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "create directory failed"))
			return
		}
	}

	c.JSON(http.StatusOK, createNodeResponse{
		Path: relPath(wsRoot, abs),
		Type: req.Type,
	})
}

// renameRequest is the JSON body for PUT /api/v1/workspace/files/*path.
type renameRequest struct {
	NewPath   string `json:"new_path"`
	Overwrite bool   `json:"overwrite"`
}

// renameResponse is the JSON body for PUT /api/v1/workspace/files/*path.
type renameResponse struct {
	OldPath string `json:"old_path"`
	NewPath string `json:"new_path"`
}

// Rename handles PUT /api/v1/workspace/files/*path. It renames or moves a file
// or directory within the user's workspace. When new_path resolves to an
// existing directory the source is moved inside it as new_path/<basename(src)>
// (the "drag into a folder" gesture). Otherwise the destination must not already
// exist; if it does the operation returns 409 unless the request sets
// "overwrite": true, in which case an existing file destination is atomically
// replaced (overwriting a directory is rejected with 409 DEST_NOT_EMPTY — tree
// merge is not supported). No changes are made on any rejection.
func (h *WorkspaceHandler) Rename(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	var req renameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", err.Error()))
		return
	}
	if strings.TrimSpace(req.NewPath) == "" {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "new_path is required"))
		return
	}

	wsRoot := h.fsSvc.UserWorkspace(userID)
	relSrc := strings.TrimPrefix(c.Param("path"), "/")
	relDst := req.NewPath

	srcAbs, err := xizhi.ValidatePathAllowReserved(wsRoot, relSrc)
	if err != nil {
		writeForbidden(c, "path outside workspace")
		return
	}
	dstAbs, err := xizhi.ValidatePathAllowReserved(wsRoot, relDst)
	if err != nil {
		writeForbidden(c, "path outside workspace")
		return
	}

	srcInfo, err := os.Stat(srcAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "source not found"))
			return
		}
		logWS(ctx, "workspace.rename.stat", srcAbs, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "stat source failed"))
		return
	}

	// Refuse to rename the workspace root itself.
	if rootInfo, rerr := os.Stat(wsRoot); rerr == nil && os.SameFile(srcInfo, rootInfo) {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "cannot rename the workspace root"))
		return
	}

	// Resolve the final destination, applying move-into-folder semantics when
	// new_path names an existing directory: the source moves inside it as
	// new_path/<basename(src)> (the "drag into a folder" gesture). Previously
	// this case returned 409.
	dstStat, err := os.Stat(dstAbs)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		logWS(ctx, "workspace.rename.stat", dstAbs, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "stat destination failed"))
		return
	}
	if err == nil && dstStat.IsDir() {
		// Destination is an existing directory → move inside it, then re-stat
		// the recomputed path so the existence/overwrite check runs against
		// dir/<basename> (which may itself already exist).
		dstAbs = filepath.Join(dstAbs, filepath.Base(srcAbs))
		dstStat, err = os.Stat(dstAbs)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			logWS(ctx, "workspace.rename.stat", dstAbs, err)
			c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "stat destination failed"))
			return
		}
	}
	if err == nil {
		// The final destination already exists. Without overwrite this is a
		// conflict (the prior behavior for an existing file); with overwrite a
		// file is atomically replaced below, but a directory is rejected —
		// merging two trees is out of scope.
		if !req.Overwrite {
			c.JSON(http.StatusConflict, errorBody("ALREADY_EXISTS", "destination already exists"))
			return
		}
		if dstStat.IsDir() {
			c.JSON(http.StatusConflict, errorBody("DEST_NOT_EMPTY", "destination directory is not empty; merge not supported"))
			return
		}
	}

	// Ensure the destination parent directory exists so moves into new
	// subdirectories succeed.
	if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
		logWS(ctx, "workspace.rename.mkdir", filepath.Dir(dstAbs), err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "create destination directory failed"))
		return
	}

	if err := os.Rename(srcAbs, dstAbs); err != nil {
		logWS(ctx, "workspace.rename.move", srcAbs, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "rename failed"))
		return
	}

	c.JSON(http.StatusOK, renameResponse{
		OldPath: relPath(wsRoot, srcAbs),
		NewPath: relPath(wsRoot, dstAbs),
	})
}

// Delete handles DELETE /api/v1/workspace/files/*path. It removes a file or,
// for a directory, recursively removes the directory and everything beneath it.
// Path validation reuses xizhi.ValidatePathAllowReserved so traversal / symlink escape is
// rejected with 403 exactly as the read endpoints enforce. Workspace files have
// no database source table, so nothing is archived — the entry is removed from
// the filesystem only.
//
// The workspace root itself is never deleted: a path that cleans to "." (e.g.
// ".", "./", "foo/..") resolves to the root and would irreversibly wipe the
// entire workspace, so it is rejected with 400. Individual files and
// subdirectories remain fully deletable.
func (h *WorkspaceHandler) Delete(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	wsRoot := h.fsSvc.UserWorkspace(userID)
	rel := c.Param("path")

	abs, err := xizhi.ValidatePathAllowReserved(wsRoot, strings.TrimPrefix(rel, "/"))
	if err != nil {
		writeForbidden(c, "path outside workspace")
		return
	}

	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "file not found"))
			return
		}
		logWS(ctx, "workspace.delete.stat", abs, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "stat file failed"))
		return
	}

	// Refuse to delete the workspace root. SameFile compares device/inode so the
	// check holds even when wsRoot sits behind a symlink (e.g. /var → /private/var
	// on macOS) or is expressed as a relative path.
	if rootInfo, rerr := os.Stat(wsRoot); rerr == nil && os.SameFile(info, rootInfo) {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "cannot delete the workspace root"))
		return
	}

	if err := os.RemoveAll(abs); err != nil {
		logWS(ctx, "workspace.delete.remove", abs, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "delete failed"))
		return
	}

	c.Status(http.StatusNoContent)
}

// OnlyOffice status codes from the DocumentServer save callback. 2 = document
// is ready for saving (being closed with changes); 6 = force save (timed or
// manual). Both carry the edited file at body.url and must be persisted.
const (
	onlyOfficeStatusSave    = 2
	onlyOfficeStatusForce   = 6
	onlyOfficeDownloadLimit = 60 * time.Second
)

// errOnlyOfficeHostNotAllowed signals a callback result URL whose host is not
// the configured DocumentServer host (an SSRF attempt or a siteUrl mismatch).
var errOnlyOfficeHostNotAllowed = errors.New("onlyoffice: result url host not in allowlist")

// onlyOfficeConfigResponse is the JSON body returned by the editor-config
// endpoint: the browser loads api.js from ServerURL and instantiates
// DocsAPI.DocEditor(id, {...config, token}) for whichever mode it needs. Edit
// and View carry their own config + token: OnlyOffice signs the whole config,
// so the frontend cannot switch modes by mutating a config in place — it must
// use the pre-signed edit or view token.
type onlyOfficeConfigResponse struct {
	ServerURL string               `json:"server_url"`
	Edit      onlyOfficeModeConfig `json:"edit"`
	View      onlyOfficeModeConfig `json:"view"`
}

// onlyOfficeModeConfig pairs one DocEditor config (edit or view) with the HS256
// JWT signing exactly that config.
type onlyOfficeModeConfig struct {
	Config map[string]any `json:"config"`
	Token  string         `json:"token"`
}

// OnlyOfficeConfig handles GET /api/v1/workspace/files/*path/onlyoffice-config.
// It builds edit and view DocEditor configs (sharing one per-request random
// document.key, document.url, and callbackUrl), signs each with the configured
// OnlyOffice secret, and returns both {config, token} pairs so the frontend can
// open the file in either mode without a second round-trip.
//
// 503 when OnlyOffice is not configured (empty secret); 403 on path escape; 404
// when the file does not exist; 400 when the path is a directory.
func (h *WorkspaceHandler) OnlyOfficeConfig(c *gin.Context) {
	if !h.oo.configured() {
		c.JSON(http.StatusServiceUnavailable, errorBody("ONLYOFFICE_DISABLED", "onlyoffice is not configured"))
		return
	}
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	wsRoot := h.fsSvc.UserWorkspace(userID)
	rel := strings.TrimPrefix(c.Param("path"), "/")

	abs, err := xizhi.ValidatePathAllowReserved(wsRoot, rel)
	if err != nil {
		writeForbidden(c, "path outside workspace")
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "file not found"))
			return
		}
		logWS(ctx, "workspace.onlyoffice_config.stat", abs, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "stat file failed"))
		return
	}
	if info.IsDir() {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "path is a directory"))
		return
	}

	// The DocumentServer reaches the backend as the requesting user: embed the
	// same Bearer JWT (still valid for its remaining lifetime) as the query token
	// on document.url and the save callbackUrl. AuthMiddleware already verified
	// it, so it is present.
	userJWT := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))

	editCfg, viewCfg, err := h.buildOnlyOfficeConfigs(rel, userJWT)
	if err != nil {
		logWS(ctx, "workspace.onlyoffice_config.build", abs, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "build editor config failed"))
		return
	}
	// Sign each config with its own token: OnlyOffice verifies the whole config,
	// so edit and view (different permissions/mode) must carry separate signatures.
	editToken, ok := h.signOnlyOfficeConfig(c, ctx, abs, editCfg)
	if !ok {
		return
	}
	viewToken, ok := h.signOnlyOfficeConfig(c, ctx, abs, viewCfg)
	if !ok {
		return
	}

	c.JSON(http.StatusOK, onlyOfficeConfigResponse{
		ServerURL: h.oo.ServerURL,
		Edit:      onlyOfficeModeConfig{Config: editCfg, Token: editToken},
		View:      onlyOfficeModeConfig{Config: viewCfg, Token: viewToken},
	})
}

// signOnlyOfficeConfig signs a DocEditor config with the OnlyOffice secret and
// replies 500 on failure. It returns ok=false when the caller must abort.
func (h *WorkspaceHandler) signOnlyOfficeConfig(c *gin.Context, ctx context.Context, abs string, cfg map[string]any) (string, bool) {
	token, err := jwt.SignClaims(h.oo.Secret, cfg)
	if err != nil {
		logWS(ctx, "workspace.onlyoffice_config.sign", abs, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "sign editor config failed"))
		return "", false
	}
	return token, true
}

// onlyOfficeVersionConfigResponse is the JSON body returned by the version-view
// editor-config endpoint. It mirrors the shape of one mode of
// onlyOfficeConfigResponse (a {config, token} pair nested under "view"), but
// returns ONLY the view mode: a historical version is immutable and has no edit
// semantics. The frontend can thus reuse its existing resp.view.{config, token}
// consumption path unchanged.
type onlyOfficeVersionConfigResponse struct {
	ServerURL string               `json:"server_url"`
	View      onlyOfficeModeConfig `json:"view"`
}

// OnlyOfficeVersionConfig handles GET /api/v1/workspace/files/*path/onlyoffice-version-config?versionId=<vid>.
// It builds a view-only DocEditor config pointing at a specific historical
// version stored in the external office-vers service (MinIO-backed, no auth),
// signs it with the OnlyOffice secret, and returns {server_url, view:{config, token}}.
//
// Unlike OnlyOfficeConfig (the live-file endpoint): document.url targets
// office-vers ({version_service_url}/documents/{userUUID}/{path}?action=version&versionId=<vid>)
// and carries NO credential (office-vers is unauthenticated by design); the
// document.key is derived deterministically from (path, versionId) so OnlyOffice
// caches and shares the conversion across opens/users (a historical version is
// immutable, so a stable key never serves stale content); and the config carries
// no callbackUrl (nothing to save back to an immutable version).
//
// 503 when the OnlyOffice secret or version_service_url is not configured; 400
// when versionId is missing; 403 on path escape. The local workspace file is NOT
// stat-checked — the version's source of truth is office-vers, so a missing local
// file must not block previewing a version that exists there (lazy validation).
func (h *WorkspaceHandler) OnlyOfficeVersionConfig(c *gin.Context) {
	if !h.oo.versionConfigured() {
		c.JSON(http.StatusServiceUnavailable, errorBody("ONLYOFFICE_DISABLED", "onlyoffice is not configured"))
		return
	}
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	versionID := strings.TrimSpace(c.Query("versionId"))
	if versionID == "" {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "versionId is required"))
		return
	}

	wsRoot := h.fsSvc.UserWorkspace(userID)
	rel := strings.TrimPrefix(c.Param("path"), "/")
	abs, err := xizhi.ValidatePathAllowReserved(wsRoot, rel)
	if err != nil {
		writeForbidden(c, "path outside workspace")
		return
	}

	cfg := h.buildOnlyOfficeVersionConfig(rel, versionID, userID)
	token, ok := h.signOnlyOfficeConfig(c, ctx, abs, cfg)
	if !ok {
		return
	}

	c.JSON(http.StatusOK, onlyOfficeVersionConfigResponse{
		ServerURL: h.oo.ServerURL,
		View:      onlyOfficeModeConfig{Config: cfg, Token: token},
	})
}

// It receives OnlyOffice document-save callbacks from the DocumentServer. For
// save statuses (2/6) it downloads the result URL — host-allowlisted to the
// configured DocumentServer to mitigate SSRF — and atomically overwrites the
// workspace file (capped at maxUploadBytes). Non-save statuses (1/3/4/7) and
// missing-URL saves are acked without touching disk. Every business outcome is
// reported with OnlyOffice's {"error": N} convention (0 ok, non-0 retry); auth
// (401) and path-escape (403) remain HTTP-level errors.
func (h *WorkspaceHandler) OnlyOfficeCallback(c *gin.Context) {
	if !h.oo.configured() {
		c.JSON(http.StatusServiceUnavailable, errorBody("ONLYOFFICE_DISABLED", "onlyoffice is not configured"))
		return
	}
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	rel := strings.TrimSpace(c.Query("path"))
	wsRoot := h.fsSvc.UserWorkspace(userID)
	abs, err := xizhi.ValidatePathAllowReserved(wsRoot, rel)
	if err != nil {
		writeForbidden(c, "path outside workspace")
		return
	}

	var body struct {
		Status int    `json:"status"`
		Key    string `json:"key"`
		URL    string `json:"url"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		// Malformed callback — reply in-contract so OnlyOffice retries rather
		// than treating a 4xx as terminal.
		writeOnlyOfficeError(c, 1)
		return
	}

	// Non-save statuses: ack without persisting. OnlyOffice opens/closes with
	// no changes, force-save-without-url, etc.
	if !onlyOfficeIsSaveStatus(body.Status) {
		writeOnlyOfficeError(c, 0)
		return
	}
	if strings.TrimSpace(body.URL) == "" {
		writeOnlyOfficeError(c, 0)
		return
	}

	// SSRF mitigation: the result URL comes from the request body, so only
	// download from the configured DocumentServer host.
	if !h.onlyOfficeURLAllowed(body.URL) {
		logWS(ctx, "workspace.onlyoffice_callback.host_rejected", body.URL, errOnlyOfficeHostNotAllowed)
		writeOnlyOfficeError(c, 1)
		return
	}

	if err := h.onlyOfficePersist(ctx, abs, body.URL); err != nil {
		logWS(ctx, "workspace.onlyoffice_callback.persist", abs, err)
		writeOnlyOfficeError(c, 1)
		return
	}
	writeOnlyOfficeError(c, 0)
}

// onlyOfficeIsSaveStatus reports whether a status warrants downloading + persisting.
func onlyOfficeIsSaveStatus(status int) bool {
	return status == onlyOfficeStatusSave || status == onlyOfficeStatusForce
}

// buildOnlyOfficeConfigs constructs the edit and view DocEditor configs for a
// workspace file. Both share one freshly random document.key (same file, same
// open, one document identity), the same documentType/title/url, and the same
// callbackUrl + user. Only the document permissions and editorConfig differ by
// mode: edit allows editing and forcesave; view is read-only and carries no
// forcesave (OnlyOffice omits save-status callbacks in view mode, so the shared
// callbackUrl is harmless there). The random key guarantees reopening always
// re-converts and never serves a stale cached document.
func (h *WorkspaceHandler) buildOnlyOfficeConfigs(rel, userJWT string) (edit, view map[string]any, err error) {
	key, err := randomOnlyOfficeKey()
	if err != nil {
		return nil, nil, err
	}
	title := filepath.Base(rel)
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(rel), "."))

	// The DocumentServer downloads the source file and POSTs saves back to the
	// backend over InternalBackend (the container-reachable origin), authenticating
	// as the user via the embedded JWT query token.
	docURL := h.oo.InternalBackend + "/api/v1/workspace/files/download/" + url.PathEscape(rel) + "?inline=1&token=" + url.QueryEscape(userJWT)
	callbackURL := h.oo.InternalBackend + "/api/v1/workspace/onlyoffice-callback?path=" + url.QueryEscape(rel) + "&token=" + url.QueryEscape(userJWT)

	documentType := onlyOfficeDocumentType(ext)
	user := gin.H{"id": "blowball", "name": "blowball"}

	// makeDoc builds the document map for a mode. All fields are shared except
	// permissions.edit; building a fresh map per mode avoids accidental aliasing.
	makeDoc := func(editAllowed bool) gin.H {
		return gin.H{
			"fileType": ext,
			"key":      key,
			"title":    title,
			"url":      docURL,
			"permissions": gin.H{
				"edit":     editAllowed,
				"download": true,
			},
		}
	}

	edit = map[string]any{
		"documentType": documentType,
		"document":     makeDoc(true),
		"editorConfig": gin.H{
			"mode":        "edit",
			"callbackUrl": callbackURL,
			"customization": gin.H{
				"forcesave": true,
			},
			"user": user,
		},
	}
	view = map[string]any{
		"documentType": documentType,
		"document":     makeDoc(false),
		"editorConfig": gin.H{
			"mode":        "view",
			"callbackUrl": callbackURL,
			"user":        user,
		},
	}
	return edit, view, nil
}

// buildOnlyOfficeVersionConfig constructs a view-only DocEditor config for a
// specific historical version stored in the external office-vers service. Unlike
// buildOnlyOfficeConfigs (the live-file endpoint):
//   - document.url targets office-vers and carries NO credential (office-vers is
//     unauthenticated by design; the document bytes come from MinIO, not the backend);
//   - document.key is DERIVED deterministically from (rel, versionID) so OnlyOffice
//     caches and shares the conversion — a historical version is immutable, so a
//     stable key never serves stale content (contrast the live endpoint's per-open
//     random key, which forces a reconvert precisely because the file may have changed);
//   - there is no callbackUrl (an immutable version has no save semantics) and no
//     customization.forcesave.
func (h *WorkspaceHandler) buildOnlyOfficeVersionConfig(rel, versionID, userUUID string) map[string]any {
	title := filepath.Base(rel)
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(rel), "."))

	key := deriveOnlyOfficeVersionKey(rel, versionID)
	docURL := h.oo.VersionServiceURL + "/documents/" + url.PathEscape(userUUID) + "/" + escapeOnlyOfficePath(rel) + "?action=version&versionId=" + url.QueryEscape(versionID)

	return map[string]any{
		"documentType": onlyOfficeDocumentType(ext),
		"document": gin.H{
			"fileType": ext,
			"key":      key,
			"title":    title,
			"url":      docURL,
			"permissions": gin.H{
				"edit":     false,
				"download": true,
			},
		},
		"editorConfig": gin.H{
			"mode": "view",
			"user": gin.H{"id": "blowball", "name": "blowball"},
		},
	}
}

// deriveOnlyOfficeVersionKey returns a deterministic document key for a given
// (logical path, version id): base32(sha256(path + ":" + versionID)), lowercase
// and unpadded to match randomOnlyOfficeKey's encoding. A historical MinIO
// version is immutable, so a stable key lets OnlyOffice cache and share the
// conversion across opens and users without ever serving stale content; a
// different versionId yields a different key. versionId is a MinIO UUID (no ':'),
// so the separator is cosmetic (no collision risk).
func deriveOnlyOfficeVersionKey(rel, versionID string) string {
	sum := sha256.Sum256([]byte(rel + ":" + versionID))
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))
}

// escapeOnlyOfficePath percent-escapes each "/"-separated segment of a logical
// path for use in a URL path, preserving the literal separators. Escaping per
// segment (rather than the whole path) keeps the directory structure visible to
// office-vers's *filepath catch-all and avoids %2F ambiguity.
func escapeOnlyOfficePath(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// onlyOfficeDocumentType maps an extension (no dot) to OnlyOffice's documentType
// word/cell/slide. Office files always carry a known extension; unknown types
// fall back to "word" and OnlyOffice rejects them client-side regardless.
func onlyOfficeDocumentType(ext string) string {
	switch ext {
	case "doc", "docx":
		return "word"
	case "xls", "xlsx":
		return "cell"
	case "ppt", "pptx":
		return "slide"
	default:
		return "word"
	}
}

// randomOnlyOfficeKey returns a fresh, URL-safe document key. OnlyOffice caches
// converted documents by key, so a random key per open guarantees a re-convert
// (no stale post-save content). crypto/rand gives 128 bits of entropy; base32
// (no padding, lowercased) keeps it under the 128-char limit and URL-safe.
func randomOnlyOfficeKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate onlyoffice key: %w", err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])), nil
}

// onlyOfficeURLAllowed reports whether a callback result URL's host matches the
// configured DocumentServer host. Comparing hostnames (not host:port) keeps a
// port drift between browser-facing and container-internal URLs from rejecting
// legitimate saves while still blocking SSRF to other hosts.
func (h *WorkspaceHandler) onlyOfficeURLAllowed(raw string) bool {
	server, err := url.Parse(h.oo.ServerURL)
	if err != nil || server.Hostname() == "" {
		return false
	}
	target, err := url.Parse(raw)
	if err != nil || target.Hostname() == "" {
		return false
	}
	return target.Hostname() == server.Hostname()
}

// errWriteTooLarge is returned by the atomic-write helpers when the input
// exceeds the configured byte limit. Callers distinguish it from ordinary I/O
// errors so they can map it to 413 FILE_TOO_LARGE (or the OnlyOffice error
// convention) rather than a generic 500.
var errWriteTooLarge = errors.New("content exceeds size limit")

// atomicWriteFromReader stages r into a freshly created temp file alongside abs
// and atomically renames it over abs on success. Creating the temp in
// filepath.Dir(abs) guarantees it shares the target's filesystem, so os.Rename
// is atomic (no EXDEV) and a crash mid-write never leaves a truncated
// destination — the previous file (if any) stays untouched until the rename.
//
// When limit > 0, at most limit bytes are accepted: the copy is bounded to
// limit+1 bytes so an overflow is detectable without buffering the whole stream,
// and anything larger is rejected with an error wrapping errWriteTooLarge (the
// temp is removed and abs is left as-is). A non-positive limit disables the cap;
// the caller must then bound r some other way (the OnlyOffice download path
// refuses an unconfigured limit before reaching here). It returns the byte count
// written, which may exceed limit on overflow before the temp is discarded.
func atomicWriteFromReader(abs string, r io.Reader, limit int64) (int64, error) {
	dir := filepath.Dir(abs)
	tmp, err := os.CreateTemp(dir, ".ws-write-*")
	if err != nil {
		return 0, fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	var n int64
	if limit > 0 {
		// Copy up to limit+1 bytes so an oversize input is detectable without
		// buffering the whole thing in memory.
		n, err = io.Copy(tmp, io.LimitReader(r, limit+1))
	} else {
		n, err = io.Copy(tmp, r)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		cleanup()
		return n, fmt.Errorf("write temp: %w", err)
	}
	if limit > 0 && n > limit {
		cleanup()
		return n, fmt.Errorf("%w: %d bytes (limit %d)", errWriteTooLarge, n, limit)
	}

	if err := os.Rename(tmpName, abs); err != nil {
		cleanup()
		return n, fmt.Errorf("rename temp: %w", err)
	}
	return n, nil
}

// atomicWriteFile writes data to abs atomically via atomicWriteFromReader. It is
// the create-or-replace primitive used by the text-content write endpoint: a
// missing target is created (the caller ensures the parent directory exists),
// an existing file is fully replaced, and any failure leaves the original
// untouched. When limit > 0, data larger than limit is rejected up front —
// before a temp is created — with an error wrapping errWriteTooLarge. It returns
// the number of bytes written.
func atomicWriteFile(abs string, data []byte, limit int64) (int, error) {
	if limit > 0 && int64(len(data)) > limit {
		return 0, errWriteTooLarge
	}
	n, err := atomicWriteFromReader(abs, bytes.NewReader(data), limit)
	return int(n), err
}

// onlyOfficePersist downloads the edited document and atomically overwrites abs.
// It streams the download straight into the shared atomicWriteFromReader
// primitive, which stages a temp file in the same directory (so os.Rename is
// atomic on the same filesystem), caps the size at maxUploadBytes, and only
// renames on full success — any failure leaves the original file untouched and
// the temp removed.
func (h *WorkspaceHandler) onlyOfficePersist(ctx context.Context, abs, fileURL string) error {
	client := &http.Client{Timeout: onlyOfficeDownloadLimit}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download result: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("download result: status %d", resp.StatusCode)
	}

	limit := h.maxUploadBytes
	if limit <= 0 {
		// No upload cap configured — refuse an unbounded download rather than
		// filling disk. This mirrors the upload path's "non-positive disables"
		// caveat (not recommended for production).
		return fmt.Errorf("download result: max upload bytes is not configured")
	}

	if _, err := atomicWriteFromReader(abs, resp.Body, limit); err != nil {
		return fmt.Errorf("download result: %w", err)
	}
	return nil
}

// writeOnlyOfficeError replies with OnlyOffice's {"error": N} convention over
// HTTP 200. OnlyOffice interprets the JSON body, not the HTTP status, as the
// success signal (0 = ok, non-0 = retry).
func writeOnlyOfficeError(c *gin.Context, code int) {
	c.JSON(http.StatusOK, gin.H{"error": code})
}

// maxUploadBytesMemory returns the in-memory threshold for multipart parsing.
// We buffer at most 32 MiB in memory; anything larger spills to disk where the
// MaxBytesReader still caps the total size. The 32 MiB default is generous for
// source files and small assets without risking runaway heap pressure.
func (h *WorkspaceHandler) maxUploadBytesMemory() int64 {
	const defaultMem = 32 << 20
	if h.maxUploadBytes > 0 && h.maxUploadBytes < defaultMem {
		return h.maxUploadBytes
	}
	return defaultMem
}

// resolveListTarget resolves the listing target for a (possibly empty) rel
// subdirectory. It uses the same xizhi.ValidatePathAllowReserved security primitive so a
// path that escapes the workspace is rejected identically to read/write. The
// empty rel case lists the workspace root directly.
func resolveListTarget(wsRoot, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return wsRoot, nil
	}
	return xizhi.ValidatePathAllowReserved(wsRoot, rel)
}

// resolveUploadDir resolves the destination directory for an upload. relDir
// may be empty (root) or a subdirectory; traversal/escape is rejected via
// xizhi.ValidatePathAllowReserved. The returned path may not yet exist; the caller creates
// it with MkdirAll.
func resolveUploadDir(wsRoot, relDir string) (string, error) {
	relDir = strings.TrimSpace(relDir)
	if relDir == "" {
		return wsRoot, nil
	}
	return xizhi.ValidatePathAllowReserved(wsRoot, relDir)
}

// fileType maps an os.DirEntry to the wire-format type tag.
func fileType(e os.DirEntry) string {
	if e.IsDir() {
		return "dir"
	}
	return "file"
}

// isHiddenName reports whether a file or directory name should be considered
// hidden (starts with "."). This matches the definition used by the xizhi
// file-discovery tools so the REST API and agent tools behave consistently.
func isHiddenName(name string) bool {
	return name != "" && name[0] == '.'
}

// parseBoolQuery parses a query parameter string as a boolean. It returns true
// only for explicit truthy values ("true", "1"); everything else, including the
// empty string, returns false. This avoids treating malformed values as errors.
func parseBoolQuery(v string) bool {
	s := strings.ToLower(strings.TrimSpace(v))
	return s == "true" || s == "1"
}

// relPath returns relPath relative to wsRoot, falling back to abs on error.
// Used to populate the JSON "path" field with a workspace-relative path the
// client can pass back to other endpoints.
func relPath(wsRoot, abs string) string {
	r, err := filepath.Rel(wsRoot, abs)
	if err != nil {
		return abs
	}
	return r
}

// isBinary sniffs the first 1024 bytes for a NUL byte (a reliable, fast
// heuristic for distinguishing text from binary that the OpenAPI specs of
// GitHub et al. also use). A NUL byte in the prefix means the file is binary.
func isBinary(data []byte) bool {
	n := len(data)
	if n > 1024 {
		n = 1024
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// writeForbidden emits the unified 403 error body.
func writeForbidden(c *gin.Context, message string) {
	c.JSON(http.StatusForbidden, errorBody("FORBIDDEN", message))
}

// logWS emits a structured error log for a workspace operation failure.
func logWS(ctx context.Context, op, path string, err error) {
	fields := []zap.Field{
		zap.String("op", op),
		zap.String("path", path),
		zap.Error(err),
	}
	if tid := trace.FromContext(ctx); tid != "" {
		fields = append(fields, zap.String("trace_id", tid))
	}
	logger.L().Error("workspace operation failed", fields...)
}
