package handler

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	miniostore "github.com/lush/blowball/services/officevers/internal/minio"

	"github.com/gin-gonic/gin"
)

// Storage is the subset of storage operations the handlers depend on. The
// concrete *minio.Client satisfies this interface; tests may substitute a mock.
//
// The "path" argument is the client's logical path (it may contain "/"
// segments); the storage implementation derives the hashed object key from it.
type Storage interface {
	Upload(ctx context.Context, uuid, path string, reader io.Reader, size int64) (string, time.Time, error)
	ListVersions(ctx context.Context, uuid, path string) ([]miniostore.Version, error)
	DownloadLatest(ctx context.Context, uuid, path string) (*miniostore.Download, error)
	DownloadVersion(ctx context.Context, uuid, path, versionID string) (*miniostore.Download, error)
	ListDirectories(ctx context.Context) ([]string, error)
	Delete(ctx context.Context, uuid, path string) error
	Rollback(ctx context.Context, uuid, path, targetVersionID string) (string, error)
}

// Handler exposes the storage operations as HTTP endpoints.
type Handler struct {
	Store Storage
}

// New constructs a Handler backed by the given storage implementation.
func New(store Storage) *Handler {
	return &Handler{Store: store}
}

// Document dispatches file-level operations based on the HTTP method and the
// ?action= query parameter. The *filepath catch-all is taken wholesale as the
// file's logical path (leading "/" trimmed); the operation lives in the query
// string so that logical paths may freely contain "/".
//
//	method   action                 operation
//	POST     (none)                 upload a new version
//	POST     rollback               rollback to a version
//	GET      (none)                 download the latest version
//	GET      versions               list version history
//	GET      version                download a specific version (?versionId=<vid>)
//	DELETE   (none)                 delete the file and all versions
func (h *Handler) Document(c *gin.Context) {
	uuid := c.Param("uuid")
	p := strings.TrimPrefix(c.Param("filepath"), "/")
	action := c.Query("action")

	switch c.Request.Method {
	case http.MethodPost:
		switch action {
		case "":
			h.handleUpload(c, uuid, p)
		case "rollback":
			h.handleRollback(c, uuid, p)
		default:
			c.JSON(http.StatusBadRequest, apiError{Error: apiErrorBody{Message: "unsupported action: " + action}})
		}
	case http.MethodGet:
		switch action {
		case "":
			h.handleDownloadLatest(c, uuid, p)
		case "versions":
			h.handleListVersions(c, uuid, p)
		case "version":
			h.handleDownloadVersion(c, uuid, p)
		default:
			c.JSON(http.StatusBadRequest, apiError{Error: apiErrorBody{Message: "unsupported action: " + action}})
		}
	case http.MethodDelete:
		if action != "" {
			c.JSON(http.StatusBadRequest, apiError{Error: apiErrorBody{Message: "unsupported action: " + action}})
			return
		}
		h.handleDelete(c, uuid, p)
	default:
		c.JSON(http.StatusMethodNotAllowed, apiError{Error: apiErrorBody{Message: "method not allowed"}})
	}
}

func (h *Handler) handleUpload(c *gin.Context, uuid, path string) {
	if path == "" {
		writeError(c, miniostore.ErrEmptyPath)
		return
	}
	size := c.Request.ContentLength // -1 when chunked (Transfer-Encoding: chunked)
	versionID, lastModified, err := h.Store.Upload(c.Request.Context(), uuid, path, c.Request.Body, size)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, uploadResponse{VersionID: versionID, LastModified: lastModified})
}

func (h *Handler) handleDownloadLatest(c *gin.Context, uuid, path string) {
	h.download(c, uuid, path, "")
}

func (h *Handler) handleDownloadVersion(c *gin.Context, uuid, path string) {
	versionID := c.Query("versionId")
	if versionID == "" {
		writeError(c, miniostore.ErrEmptyVersionID)
		return
	}
	h.download(c, uuid, path, versionID)
}

func (h *Handler) download(c *gin.Context, uuid, path, versionID string) {
	if path == "" {
		writeError(c, miniostore.ErrEmptyPath)
		return
	}
	var (
		dl  *miniostore.Download
		err error
	)
	if versionID == "" {
		dl, err = h.Store.DownloadLatest(c.Request.Context(), uuid, path)
	} else {
		dl, err = h.Store.DownloadVersion(c.Request.Context(), uuid, path, versionID)
	}
	if err != nil {
		writeError(c, err)
		return
	}
	defer dl.Reader.Close()

	extra := map[string]string{
		"X-Version-Id":  dl.VersionID,
		"Last-Modified": dl.LastModified.UTC().Format(http.TimeFormat),
	}
	c.DataFromReader(http.StatusOK, dl.Size, dl.ContentType, dl.Reader, extra)
}

func (h *Handler) handleListVersions(c *gin.Context, uuid, path string) {
	if path == "" {
		writeError(c, miniostore.ErrEmptyPath)
		return
	}
	versions, err := h.Store.ListVersions(c.Request.Context(), uuid, path)
	if err != nil {
		writeError(c, err)
		return
	}
	out := make([]versionEntry, 0, len(versions))
	for _, v := range versions {
		out = append(out, versionEntry{
			VersionID:      v.VersionID,
			LastModified:   v.LastModified,
			Size:           v.Size,
			IsLatest:       v.IsLatest,
			IsDeleteMarker: v.IsDeleteMarker,
		})
	}
	c.JSON(http.StatusOK, versionsResponse{UUID: uuid, Path: path, Versions: out})
}

func (h *Handler) handleDelete(c *gin.Context, uuid, path string) {
	if path == "" {
		writeError(c, miniostore.ErrEmptyPath)
		return
	}
	if err := h.Store.Delete(c.Request.Context(), uuid, path); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) handleRollback(c *gin.Context, uuid, path string) {
	if path == "" {
		writeError(c, miniostore.ErrEmptyPath)
		return
	}
	var body rollbackRequest
	// Accept the target version id as JSON body {"versionId":"..."}; fall back to
	// a ?versionId= query parameter for convenience.
	if err := c.ShouldBindJSON(&body); err != nil {
		body.VersionID = ""
	}
	if body.VersionID == "" {
		body.VersionID = c.Query("versionId")
	}
	if body.VersionID == "" {
		c.JSON(http.StatusBadRequest, apiError{Error: apiErrorBody{Message: "versionId is required (JSON body or ?versionId=)"}})
		return
	}
	newVersionID, err := h.Store.Rollback(c.Request.Context(), uuid, path, body.VersionID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, rollbackResponse{VersionID: newVersionID})
}

// ListDirectories handles GET /documents, returning the top-level UUID directories.
func (h *Handler) ListDirectories(c *gin.Context) {
	dirs, err := h.Store.ListDirectories(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	if dirs == nil {
		dirs = []string{}
	}
	c.JSON(http.StatusOK, directoriesResponse{Directories: dirs})
}
