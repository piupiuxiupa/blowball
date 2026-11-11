package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/resource"
)

// ScheduledResourceService is the `.cron` resource surface used by the HTTP
// adapter. Ownership, fencing and publication are all main-service business.
type ScheduledResourceService interface {
	GetDraft(ctx context.Context, userID, taskID string) (*resource.DraftInfo, error)
	ListVersions(ctx context.Context, userID, taskID string) ([]model.ScheduledResourceVersion, error)
	Publish(ctx context.Context, userID, taskID string, expectedDraftRevision uint64, requestID string) (*model.ScheduledResourceVersion, error)
}

// ScheduledResourceHandler serves the task resource metadata/publish endpoints.
type ScheduledResourceHandler struct {
	resources ScheduledResourceService
}

// NewScheduledResourceHandler wires the handler.
func NewScheduledResourceHandler(resources ScheduledResourceService) *ScheduledResourceHandler {
	return &ScheduledResourceHandler{resources: resources}
}

type resourceVersionDTO struct {
	ResourceRevision uint64 `json:"resource_revision"`
	ManifestHash     string `json:"manifest_hash"`
	FileCount        int    `json:"file_count"`
	TotalBytes       int64  `json:"total_bytes"`
	CreateTime       string `json:"create_time"`
}

type resourcePublishRequest struct {
	ExpectedDraftRevision uint64 `json:"expected_draft_revision"`
	RequestID             string `json:"request_id"`
}

// Get handles GET /api/v1/scheduled-tasks/:task_id/resources.
func (h *ScheduledResourceHandler) Get(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, errorBody("UNAUTHORIZED", "authentication required"))
		return
	}
	ctx := c.Request.Context()
	taskID := c.Param("task_id")
	draft, err := h.resources.GetDraft(ctx, userID, taskID)
	if err != nil {
		writeResourceError(c, err)
		return
	}
	versions, err := h.resources.ListVersions(ctx, userID, taskID)
	if err != nil {
		writeResourceError(c, err)
		return
	}
	out := make([]resourceVersionDTO, 0, len(versions))
	for _, v := range versions {
		out = append(out, resourceVersionDTO{
			ResourceRevision: v.Revision, ManifestHash: v.ManifestHash,
			FileCount: v.FileCount, TotalBytes: v.TotalBytes,
			CreateTime: v.CreateTime.UTC().Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"task_id":        taskID,
		"draft_revision": draft.DraftRevision,
		"files":          draft.Files,
		"versions":       out,
	})
}

// Publish handles POST /api/v1/scheduled-tasks/:task_id/resources/publish.
func (h *ScheduledResourceHandler) Publish(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, errorBody("UNAUTHORIZED", "authentication required"))
		return
	}
	var req resourcePublishRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, scheduledTaskBodyBytes)
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, errorBody("INVALID_JSON", "invalid JSON request body"))
		return
	}
	version, err := h.resources.Publish(c.Request.Context(), userID, c.Param("task_id"),
		req.ExpectedDraftRevision, req.RequestID)
	if err != nil {
		writeResourceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"resource_revision": version.Revision,
		"manifest_hash":     version.ManifestHash,
		"file_count":        version.FileCount,
		"total_bytes":       version.TotalBytes,
	})
}
