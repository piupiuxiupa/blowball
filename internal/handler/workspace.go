package handler

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/store/fs"
	"github.com/lush/blowball/internal/tool/xizhi"
)

// WorkspaceHandler owns the /api/v1/workspace/* routes: file listing, upload,
// download, and text-content retrieval. All operations are scoped to the
// authenticated user's workspace directory and validated via xizhi.ValidatePath
// so path traversal and symlink escapes are rejected with 403.
type WorkspaceHandler struct {
	fsSvc          *fs.Store
	maxUploadBytes int64
}

// NewWorkspaceHandler wires the handler. maxUploadBytes is the per-file upload
// cap; uploads larger than this are rejected with 413 before they reach disk.
// A non-positive value disables the cap (not recommended for production).
func NewWorkspaceHandler(fsSvc *fs.Store, maxUploadBytes int64) *WorkspaceHandler {
	return &WorkspaceHandler{fsSvc: fsSvc, maxUploadBytes: maxUploadBytes}
}

// fileEntry is one element of the GET /api/v1/workspace/files response array.
type fileEntry struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	UpdateTime string `json:"update_time"`
}

// List handles GET /api/v1/workspace/files[?path=<sub>]. Returns 200 with a
// (possibly empty) array of file/dir entries sorted by name. A path that
// resolves outside the workspace is rejected with 403.
func (h *WorkspaceHandler) List(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	wsRoot := h.fsSvc.UserWorkspace(userID)
	rel := c.Query("path")

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
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, fileEntry{
			Name:       e.Name(),
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

	abs, err := xizhi.ValidatePath(wsRoot, strings.TrimPrefix(rel, "/"))
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

	abs, err := xizhi.ValidatePath(wsRoot, strings.TrimPrefix(rel, "/"))
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
// subdirectory. It uses the same xizhi.ValidatePath security primitive so a
// path that escapes the workspace is rejected identically to read/write. The
// empty rel case lists the workspace root directly.
func resolveListTarget(wsRoot, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return wsRoot, nil
	}
	return xizhi.ValidatePath(wsRoot, rel)
}

// resolveUploadDir resolves the destination directory for an upload. relDir
// may be empty (root) or a subdirectory; traversal/escape is rejected via
// xizhi.ValidatePath. The returned path may not yet exist; the caller creates
// it with MkdirAll.
func resolveUploadDir(wsRoot, relDir string) (string, error) {
	relDir = strings.TrimSpace(relDir)
	if relDir == "" {
		return wsRoot, nil
	}
	return xizhi.ValidatePath(wsRoot, relDir)
}

// fileType maps an os.DirEntry to the wire-format type tag.
func fileType(e os.DirEntry) string {
	if e.IsDir() {
		return "dir"
	}
	return "file"
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
