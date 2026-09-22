package handler

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lush/blowball/internal/artifact"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"

	"go.uber.org/zap"
)

// ArtifactHandler serves the turn-artifacts version endpoints: per-version
// content reads and path→version resolution. (Office-file version previews go
// through the existing office-vers-backed .../onlyoffice-version-config
// endpoint on WorkspaceHandler.) All lookups key off opaque version ids or validated
// workspace-relative paths; ownership is enforced by the service layer
// (cross-user version ids resolve to 404).
type ArtifactHandler struct {
	svc *artifact.Service
}

// NewArtifactHandler wires the handler.
func NewArtifactHandler(svc *artifact.Service) *ArtifactHandler {
	return &ArtifactHandler{svc: svc}
}

// Resolve handles GET /api/v1/workspace/versions/resolve?path=<rel>[&before=<RFC3339>].
// It returns the newest version record for the path, or the newest at or
// before `before` when given — the frontend's fallback for pinning links that
// reference files produced by earlier turns. 400 on a missing/invalid
// parameter, 404 when the path was never versioned.
func (h *ArtifactHandler) Resolve(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	rel := strings.TrimSpace(c.Query("path"))
	if rel == "" {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "path is required"))
		return
	}
	// Reject traversal/absolute paths: the same rule workspace paths live by.
	// The lookup itself is a DB equality match, so this is defense in depth
	// for the blob-store path shape, not query safety.
	if strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
		writeForbidden(c, "path outside workspace")
		return
	}

	var before time.Time
	if raw := strings.TrimSpace(c.Query("before")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "before must be RFC3339"))
			return
		}
		before = t
	}

	rec, err := h.svc.Resolve(ctx, userID, rel, before)
	if err != nil {
		logger.FromContext(ctx).Error("artifact resolve failed",
			zap.String("op", "handler.artifact_resolve"), zap.String("user_id", userID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "resolve failed"))
		return
	}
	if rec == nil {
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "path has no versions"))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"path":       rec.Path,
		"version_id": rec.VersionID,
		"size":       rec.Size,
		"mime":       rec.Mime,
	})
}

// VersionContent handles GET /api/v1/workspace/versions/:vid/content. It
// serves the snapshot bytes with the recorded MIME type. Auth accepts either
// the Bearer header or a ?token= query JWT (the versionContentAuthMW picks),
// so browser-native contexts (<img>, iframe, OnlyOffice document.url) work.
// Cross-user version ids and unknown ids both return 404.
func (h *ArtifactHandler) VersionContent(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	vid := strings.TrimSpace(c.Param("vid"))
	if vid == "" {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "version id is required"))
		return
	}

	rec, data, err := h.svc.OpenVersion(ctx, userID, vid)
	if err != nil {
		logger.FromContext(ctx).Error("artifact version read failed",
			zap.String("op", "handler.artifact_content"), zap.String("user_id", userID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "read version failed"))
		return
	}
	if rec == nil {
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "version not found"))
		return
	}

	mimeType := rec.Mime
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	// Content-Disposition filename keeps downloads sensible; inline lets
	// browsers render pdf/image previews directly.
	c.Header("Content-Disposition", "inline; filename*=UTF-8''"+url.PathEscape(filepath.Base(rec.Path)))
	c.Data(http.StatusOK, mimeType, data)
}
