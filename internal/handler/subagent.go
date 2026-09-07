package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/service"
)

// SubAgentHandler owns the read-only, lazily loaded sub-agent run endpoints.
// Live output remains on SSE / run events; these routes expose durable terminal
// transcripts only.
type SubAgentHandler struct {
	svc *service.SubAgentService
}

// NewSubAgentHandler wires the transcript read handler.
func NewSubAgentHandler(svc *service.SubAgentService) *SubAgentHandler {
	return &SubAgentHandler{svc: svc}
}

// listSubAgentRunsResponse is GET .../runs.
type listSubAgentRunsResponse struct {
	Runs []service.SubAgentRunSummary `json:"runs"`
}

// ListRuns handles GET /api/v1/sessions/:session_id/subagents/:agent_instance_id/runs.
func (h *SubAgentHandler) ListRuns(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	sessionID := c.Param("session_id")
	instanceID := c.Param("agent_instance_id")
	ctx := trace.WithContext(c.Request.Context(), middleware.TraceIDFromCtx(c))

	runs, err := h.svc.ListRuns(ctx, userID, sessionID, instanceID)
	if err != nil {
		writeSubAgentError(c, ctx, "subagent.list_runs", err)
		return
	}
	c.JSON(http.StatusOK, listSubAgentRunsResponse{Runs: runs})
}

// GetRun handles GET /api/v1/sessions/:session_id/subagents/:agent_instance_id/runs/:run_id.
func (h *SubAgentHandler) GetRun(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	sessionID := c.Param("session_id")
	instanceID := c.Param("agent_instance_id")
	runID := c.Param("run_id")
	ctx := trace.WithContext(c.Request.Context(), middleware.TraceIDFromCtx(c))

	run, err := h.svc.GetRun(ctx, userID, sessionID, instanceID, runID)
	if err != nil {
		writeSubAgentError(c, ctx, "subagent.get_run", err)
		return
	}
	c.JSON(http.StatusOK, run)
}

func writeSubAgentError(c *gin.Context, ctx context.Context, op string, err error) {
	if errors.Is(err, service.ErrSubAgentNotFound) {
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "sub-agent run not found"))
		return
	}
	logger.FromContext(ctx).Error(op+" failed",
		zap.String("op", op), zap.Error(err))
	c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "sub-agent transcript read failed"))
}
