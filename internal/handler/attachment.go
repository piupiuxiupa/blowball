package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// maxStagedAttachmentsPerUser bounds how many unbound (staged) images one
// user may hold at once (add-staged-attachment-upload). Staged uploads never
// get a session-delete cleanup, so this quota plus the orphan sweeper's TTL
// are the two guards against unbounded accumulation. The count is a fast
// preflight; InsertStagedAttachment performs the authoritative atomic quota
// reservation before any file is written.
const maxStagedAttachmentsPerUser = model.MaxStagedAttachmentsPerUser

// attachmentMimeExt is the shared MIME→extension table (model.AttachmentMIMEExt)
// under its historical local name.
var attachmentMimeExt = model.AttachmentMIMEExt

// attachmentStore is the registry subset the attachment and streaming
// handlers need; satisfied by *mysql.Store and fakes in tests.
type attachmentStore interface {
	InsertAttachment(ctx context.Context, a model.Attachment) error
	GetAttachment(ctx context.Context, id, userID, sessionID string) (*model.Attachment, error)
	// add-staged-attachment-upload: staged upload quota + send-time claim.
	CountStagedAttachments(ctx context.Context, userID string) (int, error)
	InsertStagedAttachment(ctx context.Context, a model.Attachment, limit int) (bool, error)
	DeleteStagedAttachment(ctx context.Context, id string) (bool, error)
	GetAttachmentByUser(ctx context.Context, id, userID string) (*model.Attachment, error)
	ClaimAttachment(ctx context.Context, id, userID, sessionID string) (bool, error)
	UnclaimAttachment(ctx context.Context, id, userID, sessionID string) error
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
//
// Kept as the "upload-and-immediately-bind" alias of the staged upload
// (add-staged-attachment-upload): same pipeline, but the row is born bound to
// the session in the URL.
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

	filename, data, mime, ok := h.readUploadedImage(c)
	if !ok {
		return
	}
	att, ok := h.persistAttachment(c, ctx, userID, h.fs.UserAttachmentDir(userID, sessionID), &sessionID, filename, data, mime)
	if !ok {
		return
	}
	respondAttachment(c, att)
}

// UploadStaged handles POST /api/v1/attachments
// (add-staged-attachment-upload): the session-independent variant that lets
// clients upload the moment an image is picked, before any session exists.
// The pipeline is identical to Upload except the row is born STAGED
// (session_id NULL) and the file lands under
// data/{user}/attachments/_staging/; the send path claims it into a session
// later. The per-user staged quota is the cost backstop for an upload
// surface that never gets a session-delete cleanup.
func (h *AttachmentHandler) UploadStaged(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	staged, err := h.store.CountStagedAttachments(ctx, userID)
	if err != nil {
		logAttachment(ctx, "attachment.upload_staged.count", userID, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "staged attachment count failed"))
		return
	}
	if staged >= maxStagedAttachmentsPerUser {
		c.JSON(http.StatusTooManyRequests, errorBody("TOO_MANY_STAGED_ATTACHMENTS",
			fmt.Sprintf("at most %d staged attachments are allowed per user", maxStagedAttachmentsPerUser)))
		return
	}

	filename, data, mime, ok := h.readUploadedImage(c)
	if !ok {
		return
	}
	att, ok := h.persistStagedAttachment(c, ctx, userID, h.fs.UserAttachmentStagingDir(userID), filename, data, mime)
	if !ok {
		return
	}
	respondAttachment(c, att)
}

// persistStagedAttachment reserves the quota row before writing bytes, so
// concurrent uploads cannot create more than the configured number of staged
// files. If the file write fails, the reservation is removed on a detached
// bounded context.
func (h *AttachmentHandler) persistStagedAttachment(c *gin.Context, ctx context.Context, userID, dir, filename string, data []byte, mime string) (model.Attachment, bool) {
	attachmentID, err := uuid.NewV7()
	if err != nil {
		logAttachment(ctx, "attachment.upload_staged.uuid", dir, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "generate attachment id failed"))
		return model.Attachment{}, false
	}
	att := model.Attachment{
		ID:       attachmentID.String(),
		UserID:   userID,
		Filename: filename,
		MIME:     mime,
		Size:     int64(len(data)),
	}
	reserved, err := h.store.InsertStagedAttachment(ctx, att, maxStagedAttachmentsPerUser)
	if err != nil {
		logAttachment(ctx, "attachment.upload_staged.reserve", att.ID, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "reserve staged attachment failed"))
		return model.Attachment{}, false
	}
	if !reserved {
		c.JSON(http.StatusTooManyRequests, errorBody("TOO_MANY_STAGED_ATTACHMENTS",
			fmt.Sprintf("at most %d staged attachments are allowed per user", maxStagedAttachmentsPerUser)))
		return model.Attachment{}, false
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.rollbackStagedReservation(ctx, att.ID)
		logAttachment(ctx, "attachment.upload_staged.mkdir", dir, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "create attachment dir failed"))
		return model.Attachment{}, false
	}
	dst := filepath.Join(dir, att.ID+attachmentMimeExt[mime])
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		_ = os.Remove(dst)
		h.rollbackStagedReservation(ctx, att.ID)
		logAttachment(ctx, "attachment.upload_staged.write", dst, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "save attachment failed"))
		return model.Attachment{}, false
	}
	return att, true
}

func (h *AttachmentHandler) rollbackStagedReservation(ctx context.Context, attachmentID string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := h.store.DeleteStagedAttachment(cleanupCtx, attachmentID); err != nil {
		logAttachment(cleanupCtx, "attachment.upload_staged.rollback", attachmentID, err)
	}
}

// readUploadedImage validates the multipart body of one attachment upload and
// returns the raw bytes, the sniffed MIME, and the display filename. On any
// validation failure it writes the error response itself and reports ok=false
// (the error branch is identical for both upload endpoints).
func (h *AttachmentHandler) readUploadedImage(c *gin.Context) (filename string, data []byte, mime string, ok bool) {
	// Cap the body before multipart parsing (the upload-endpoint convention):
	// an oversized image is rejected as it arrives. The slack covers the
	// multipart framing overhead around the 8 MiB payload.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAttachmentBytes+(1<<20))
	if err := c.Request.ParseMultipartForm(8 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, errorBody("FILE_TOO_LARGE", "file too large"))
			return "", nil, "", false
		}
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", err.Error()))
		return "", nil, "", false
	}
	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "file field is required"))
		return "", nil, "", false
	}
	if fileHeader.Size > maxAttachmentBytes {
		c.JSON(http.StatusRequestEntityTooLarge, errorBody("FILE_TOO_LARGE", "file too large"))
		return "", nil, "", false
	}

	f, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "cannot open uploaded file"))
		return "", nil, "", false
	}
	defer f.Close()
	data, err = io.ReadAll(f)
	if err != nil {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "cannot read uploaded file"))
		return "", nil, "", false
	}
	if int64(len(data)) > maxAttachmentBytes {
		c.JSON(http.StatusRequestEntityTooLarge, errorBody("FILE_TOO_LARGE", "file too large"))
		return "", nil, "", false
	}

	mime, ok = xizhi.SniffImageMIME(data)
	if !ok {
		c.JSON(http.StatusBadRequest, errorBody("UNSUPPORTED_IMAGE_TYPE",
			"unsupported image format (supported: image/png, image/jpeg, image/webp, image/gif)"))
		return "", nil, "", false
	}
	return sanitizeAttachmentFilename(fileHeader.Filename), data, mime, true
}

// persistAttachment writes the image bytes under dir and registers the row.
// sessionID nil inserts a STAGED row (add-staged-attachment-upload). On any
// failure it writes the error response and reports ok=false; a failed
// registry insert also removes the just-written file so disk and table never
// disagree about which ids exist.
func (h *AttachmentHandler) persistAttachment(c *gin.Context, ctx context.Context, userID, dir string, sessionID *string, filename string, data []byte, mime string) (model.Attachment, bool) {
	attachmentID, err := uuid.NewV7()
	if err != nil {
		logAttachment(ctx, "attachment.upload.uuid", dir, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "generate attachment id failed"))
		return model.Attachment{}, false
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		logAttachment(ctx, "attachment.upload.mkdir", dir, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "create attachment dir failed"))
		return model.Attachment{}, false
	}
	dst := filepath.Join(dir, attachmentID.String()+attachmentMimeExt[mime])
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		logAttachment(ctx, "attachment.upload.write", dst, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "save attachment failed"))
		return model.Attachment{}, false
	}

	att := model.Attachment{
		ID:        attachmentID.String(),
		UserID:    userID,
		SessionID: sessionID,
		Filename:  filename,
		MIME:      mime,
		Size:      int64(len(data)),
	}
	if err := h.store.InsertAttachment(ctx, att); err != nil {
		_ = os.Remove(dst)
		logAttachment(ctx, "attachment.upload.insert", att.ID, err)
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "register attachment failed"))
		return model.Attachment{}, false
	}
	return att, true
}

// respondAttachment renders the shared upload response (identical for the
// session-scoped and the staged endpoint).
func respondAttachment(c *gin.Context, att model.Attachment) {
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
	// RFC 6266/5987 encoding (ASCII fallback + filename*): raw or non-ASCII
	// client filenames must never produce a malformed header.
	c.Header("Content-Disposition", contentDisposition(att.Filename, true))
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

// attachmentPlaceholder renders the deterministic text stand-in for one
// attachment in MODEL-facing history (add-image-attachments). The format is
// fixed and must never change between turns: once a past turn's image is
// rendered as a placeholder, every later turn re-renders the same bytes so
// the append-only replay contract (and provider prefix caching from that
// point on) holds.
func attachmentPlaceholder(filename string) string {
	return "[图片: " + filename + "]"
}

// renderAttachmentPlaceholders appends one placeholder line per attachment to
// content, preserving the original text verbatim. It is the single renderer
// for history reconstruction and the current-turn missing-file fallback.
func renderAttachmentPlaceholders(content string, filenames []string) string {
	if len(filenames) == 0 {
		return content
	}
	var b strings.Builder
	b.WriteString(content)
	for _, f := range filenames {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(attachmentPlaceholder(f))
	}
	return b.String()
}

// attachmentFilenames projects refs onto their display filenames for the
// placeholder renderer, preserving the reference order.
func attachmentFilenames(refs []model.MessageAttachmentRef) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Filename
	}
	return out
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
