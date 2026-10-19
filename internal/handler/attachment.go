package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/store/fs"
	"github.com/lush/blowball/internal/tool/xizhi"
	"go.uber.org/zap"
)

// maxAttachmentBytes is the per-image upload cap (8 MiB), aligned with the
// xizhi_view_image sidecar's default so one deployment-wide budget covers
// every path an image can take into a model call.
const maxAttachmentBytes = 8 << 20

// maxAttachmentsPerMessage bounds how many images one message may carry —
// both a cost backstop and a guard against gateway per-request image limits.
const maxAttachmentsPerMessage = 4

// attachmentMimeExt maps the four sniffed image formats onto the on-disk
// extension. The extension derives from the SNIFFED type (never the client
// filename), so the stored file always matches its bytes.
var attachmentMimeExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// attachmentStore is the registry subset AttachmentHandler needs; satisfied
// by *mysql.Store and fakes in tests.
type attachmentStore interface {
	InsertAttachment(ctx context.Context, a model.Attachment) error
	GetAttachment(ctx context.Context, id, userID, sessionID string) (*model.Attachment, error)
}

// sessionGetter is the session-ownership subset needed here; satisfied by
// *service.SessionService and fakes in tests.
type sessionGetter interface {
	GetSessionByID(ctx context.Context, sessionID string) (*model.Session, error)
}

// AttachmentHandler owns the chat image attachment endpoints
// (add-image-attachments): the session-scoped multipart upload (Bearer) and
// the query-token content read that <img> elements use.
type AttachmentHandler struct {
	store    attachmentStore
	sessions sessionGetter
	fs       *fs.Store
}

// NewAttachmentHandler wires the handler with the registry store, the session
// ownership reader, and the per-user filesystem tree.
func NewAttachmentHandler(store attachmentStore, sessions sessionGetter, fsStore *fs.Store) *AttachmentHandler {
	return &AttachmentHandler{store: store, sessions: sessions, fs: fsStore}
}

// Upload handles POST /api/v1/sessions/:session_id/attachments. The multipart
// body carries one "file" field. The MIME type is sniffed from the file
// header (the client-declared content type and extension are ignored), the
// size is capped at 8 MiB, and the bytes land at
// data/{user}/attachments/{session}/{uuid}.{ext} — the original filename is
// metadata only and never participates in the path.
func (h *AttachmentHandler) Upload(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	sessionID := c.Param("session_id")
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	sess, err := h.sessions.GetSessionByID(ctx, sessionID)
	if err != nil {
		logAttachment(ctx, "attachment.upload.session", sessionID, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "session lookup failed"))
		return
	}
	if sess == nil || sess.UserID != userID {
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "session not found"))
		return
	}

	// Cap the body before multipart parsing (the upload-endpoint convention):
	// an oversized image is rejected as it arrives. The slack covers the
	// multipart framing overhead around the 8 MiB payload.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAttachmentBytes+(1<<20))
	if err := c.Request.ParseMultipartForm(8 << 20); err != nil {
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
	if fileHeader.Size > maxAttachmentBytes {
		c.JSON(http.StatusRequestEntityTooLarge, errorBody("FILE_TOO_LARGE", "file too large"))
		return
	}

	f, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "cannot open uploaded file"))
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "cannot read uploaded file"))
		return
	}
	if int64(len(data)) > maxAttachmentBytes {
		c.JSON(http.StatusRequestEntityTooLarge, errorBody("FILE_TOO_LARGE", "file too large"))
		return
	}

	mime, ok := xizhi.SniffImageMIME(data)
	if !ok {
		c.JSON(http.StatusBadRequest, errorBody("UNSUPPORTED_IMAGE_TYPE",
			"unsupported image format (supported: image/png, image/jpeg, image/webp, image/gif)"))
		return
	}

	attachmentID, err := uuid.NewV7()
	if err != nil {
		logAttachment(ctx, "attachment.upload.uuid", sessionID, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "generate attachment id failed"))
		return
	}

	dir := h.fs.UserAttachmentDir(userID, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logAttachment(ctx, "attachment.upload.mkdir", dir, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "create attachment dir failed"))
		return
	}
	dst := filepath.Join(dir, attachmentID.String()+attachmentMimeExt[mime])
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		logAttachment(ctx, "attachment.upload.write", dst, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "save attachment failed"))
		return
	}

	att := model.Attachment{
		ID:        attachmentID.String(),
		UserID:    userID,
		SessionID: sessionID,
		Filename:  sanitizeAttachmentFilename(fileHeader.Filename),
		MIME:      mime,
		Size:      int64(len(data)),
	}
	if err := h.store.InsertAttachment(ctx, att); err != nil {
		// Registry insert failed — remove the just-written file so disk and
		// table never disagree about which ids exist.
		_ = os.Remove(dst)
		logAttachment(ctx, "attachment.upload.insert", att.ID, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "register attachment failed"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"attachment_id": att.ID,
		"filename":      att.Filename,
		"mime":          att.MIME,
		"size":          att.Size,
	})
}

// Content handles GET /api/v1/sessions/:session_id/attachments/:attachment_id/content.
// It authenticates via the URL query token (QueryTokenAuthMiddleware — the
// TokenDownload pattern) so browser-native elements like <img> can fetch it
// without an Authorization header. Attachments are immutable (a re-upload is
// a new id), so the response carries an ETag plus a one-year private
// immutable cache policy; http.ServeContent turns If-None-Match into 304.
func (h *AttachmentHandler) Content(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	sessionID := c.Param("session_id")
	attachmentID := strings.TrimSpace(c.Param("attachment_id"))
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	// The id is a UUID by construction; anything else can only be an
	// enumeration/traversal attempt. Parse before touching the filesystem.
	if _, err := uuid.Parse(attachmentID); err != nil {
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "attachment not found"))
		return
	}

	att, err := h.store.GetAttachment(ctx, attachmentID, userID, sessionID)
	if err != nil {
		logAttachment(ctx, "attachment.content.get", attachmentID, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "attachment lookup failed"))
		return
	}
	if att == nil {
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "attachment not found"))
		return
	}

	ext, ok := attachmentMimeExt[att.MIME]
	if !ok {
		// Registry rows are written only after a sniffed upload; an unknown
		// mime means a corrupted/foreign row, not a servable file.
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "attachment not found"))
		return
	}
	path := filepath.Join(h.fs.UserAttachmentDir(userID, sessionID), att.ID+ext)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "attachment not found"))
			return
		}
		logAttachment(ctx, "attachment.content.stat", path, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "stat attachment failed"))
		return
	}

	c.Header("Content-Type", att.MIME)
	c.Header("ETag", `"`+att.ID+`"`)
	c.Header("Cache-Control", "private, max-age=31536000, immutable")
	c.Header("Content-Disposition", `inline; filename="`+sanitizeAttachmentFilename(att.Filename)+`"`)
	c.File(path)
}

// sanitizeAttachmentFilename reduces a client-supplied filename to a
// display-only basename, bounded to 255 bytes — it is stored for UI echo and
// Content-Disposition and never used to build a path.
func sanitizeAttachmentFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == "/" || name == `\` || name == "" {
		return "image"
	}
	if utf8.RuneCountInString(name) > 255 {
		runes := []rune(name)
		name = string(runes[:255])
	}
	return name
}

// logAttachment emits a structured error log for an attachment operation.
func logAttachment(ctx context.Context, op, path string, err error) {
	fields := []zap.Field{
		zap.String("op", op),
		zap.String("path", path),
		zap.Error(err),
	}
	if tid := trace.FromContext(ctx); tid != "" {
		fields = append(fields, zap.String("trace_id", tid))
	}
	logger.L().Error("attachment operation failed", fields...)
}
