package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/schedule"
	"github.com/lush/blowball/internal/service"
)

const scheduledTaskBodyBytes = 1 << 20

type scheduledTaskService interface {
	Create(ctx context.Context, userID string, input service.CreateScheduledTaskInput) (*model.ScheduledTask, error)
	Get(ctx context.Context, userID, taskID string) (*model.ScheduledTask, error)
	List(ctx context.Context, userID string, limit, offset int) ([]model.ScheduledTask, error)
	Update(ctx context.Context, userID, taskID string, input service.UpdateScheduledTaskInput) (*model.ScheduledTask, error)
	Delete(ctx context.Context, userID, taskID string) error
	Runs(ctx context.Context, userID, taskID string, limit, offset int) ([]model.ScheduledTaskRun, error)
}

// ScheduledTaskHandler exposes owner-only task management on the api/all
// partitions. It has no orchestrator or model client dependency.
type ScheduledTaskHandler struct {
	svc scheduledTaskService
}

func NewScheduledTaskHandler(svc scheduledTaskService) *ScheduledTaskHandler {
	return &ScheduledTaskHandler{svc: svc}
}

type scheduledTaskCreateRequest struct {
	Name            string                       `json:"name"`
	Prompt          string                       `json:"prompt"`
	Schedule        schedule.Rule                `json:"schedule"`
	Session         model.ScheduledSessionPolicy `json:"session"`
	Model           string                       `json:"model"`
	ReasoningEffort string                       `json:"reasoning_effort"`
	Enabled         *bool                        `json:"enabled"`
}

type scheduledTaskUpdateRequest struct {
	Name            *string                       `json:"name"`
	Prompt          *string                       `json:"prompt"`
	Schedule        *schedule.Rule                `json:"schedule"`
	Session         *model.ScheduledSessionPolicy `json:"session"`
	Model           *string                       `json:"model"`
	ReasoningEffort *string                       `json:"reasoning_effort"`
	Enabled         *bool                         `json:"enabled"`
}

func (h *ScheduledTaskHandler) Create(c *gin.Context) {
	userID, ok := scheduledTaskUser(c)
	if !ok {
		return
	}
	var req scheduledTaskCreateRequest
	if !decodeScheduledTaskBody(c, &req) {
		return
	}
	task, err := h.svc.Create(c.Request.Context(), userID, service.CreateScheduledTaskInput{
		Name: req.Name, Prompt: req.Prompt, Schedule: req.Schedule, Session: req.Session,
		Model: req.Model, ReasoningEffort: req.ReasoningEffort, Enabled: req.Enabled,
	})
	if err != nil {
		writeScheduledTaskError(c, err)
		return
	}
	c.JSON(http.StatusCreated, task)
}

func (h *ScheduledTaskHandler) Get(c *gin.Context) {
	userID, ok := scheduledTaskUser(c)
	if !ok {
		return
	}
	task, err := h.svc.Get(c.Request.Context(), userID, c.Param("task_id"))
	if err != nil {
		writeScheduledTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

func (h *ScheduledTaskHandler) List(c *gin.Context) {
	userID, ok := scheduledTaskUser(c)
	if !ok {
		return
	}
	limit, offset, ok := scheduledTaskPagination(c)
	if !ok {
		return
	}
	tasks, err := h.svc.List(c.Request.Context(), userID, limit, offset)
	if err != nil {
		writeScheduledTaskError(c, err)
		return
	}
	if tasks == nil {
		tasks = []model.ScheduledTask{}
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks, "limit": limit, "offset": offset})
}

func (h *ScheduledTaskHandler) Update(c *gin.Context) {
	userID, ok := scheduledTaskUser(c)
	if !ok {
		return
	}
	var req scheduledTaskUpdateRequest
	if !decodeScheduledTaskBody(c, &req) {
		return
	}
	task, err := h.svc.Update(c.Request.Context(), userID, c.Param("task_id"), service.UpdateScheduledTaskInput{
		Name: req.Name, Prompt: req.Prompt, Schedule: req.Schedule, Session: req.Session,
		Model: req.Model, ReasoningEffort: req.ReasoningEffort, Enabled: req.Enabled,
	})
	if err != nil {
		writeScheduledTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

func (h *ScheduledTaskHandler) Delete(c *gin.Context) {
	userID, ok := scheduledTaskUser(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), userID, c.Param("task_id")); err != nil {
		writeScheduledTaskError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *ScheduledTaskHandler) Runs(c *gin.Context) {
	userID, ok := scheduledTaskUser(c)
	if !ok {
		return
	}
	limit, offset, ok := scheduledTaskPagination(c)
	if !ok {
		return
	}
	runs, err := h.svc.Runs(c.Request.Context(), userID, c.Param("task_id"), limit, offset)
	if err != nil {
		writeScheduledTaskError(c, err)
		return
	}
	if runs == nil {
		runs = []model.ScheduledTaskRun{}
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs, "limit": limit, "offset": offset})
}

func scheduledTaskUser(c *gin.Context) (string, bool) {
	userID := middleware.UserIDFromCtx(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, errorBody("UNAUTHORIZED", "authentication required"))
		return "", false
	}
	return userID, true
}

func decodeScheduledTaskBody(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, scheduledTaskBodyBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, errorBody("REQUEST_TOO_LARGE", "request body exceeds 1MB"))
			return false
		}
		c.JSON(http.StatusBadRequest, errorBody("INVALID_JSON", "invalid JSON request body"))
		return false
	}
	return true
}

func scheduledTaskPagination(c *gin.Context) (int, int, bool) {
	limit, offset := 20, 0
	var err error
	if raw := c.Query("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			c.JSON(http.StatusBadRequest, errorBody("INVALID_PAGINATION", "limit must be a positive integer"))
			return 0, 0, false
		}
		if limit > 100 {
			limit = 100
		}
	}
	if raw := c.Query("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			c.JSON(http.StatusBadRequest, errorBody("INVALID_PAGINATION", "offset must be a non-negative integer"))
			return 0, 0, false
		}
	}
	return limit, offset, true
}

func writeScheduledTaskError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrScheduledTaskNotFound):
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "scheduled task not found"))
	default:
		var validation *service.ScheduledTaskError
		if errors.As(err, &validation) {
			c.JSON(http.StatusBadRequest, errorBody(validation.Code, validation.Message))
			return
		}
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "scheduled task operation failed"))
	}
}
