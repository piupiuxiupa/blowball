package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/protobuf/types/known/timestamppb"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/pkg/middleware"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/rpcx"
	"github.com/lush/blowball/pkg/schedule"
)

const scheduledTaskBodyBytes = 1 << 20

// ScheduledTaskRPC is the Scheduler management surface used by the HTTP
// adapter. There is deliberately no local service fallback: when the RPC is
// unavailable the endpoint returns 503 (scheduled-task-service spec).
type ScheduledTaskRPC interface {
	CreateTask(ctx context.Context, req *schedulerv1.CreateTaskRequest) (*schedulerv1.CreateTaskResponse, error)
	GetTask(ctx context.Context, req *schedulerv1.GetTaskRequest) (*schedulerv1.GetTaskResponse, error)
	ListTasks(ctx context.Context, req *schedulerv1.ListTasksRequest) (*schedulerv1.ListTasksResponse, error)
	UpdateTask(ctx context.Context, req *schedulerv1.UpdateTaskRequest) (*schedulerv1.UpdateTaskResponse, error)
	DeleteTask(ctx context.Context, req *schedulerv1.DeleteTaskRequest) (*schedulerv1.DeleteTaskResponse, error)
	ListOccurrences(ctx context.Context, req *schedulerv1.ListOccurrencesRequest) (*schedulerv1.ListOccurrencesResponse, error)
}

// ScheduledTaskHandler exposes owner-only task management on the api/all
// partitions by adapting HTTP to the standalone Scheduler.
type ScheduledTaskHandler struct {
	rpc           ScheduledTaskRPC
	defaultModel  string
	contentExists func(ctx context.Context, source, sessionID string) (bool, error)
}

// NewScheduledTaskHandler wires the RPC-backed handler.
func NewScheduledTaskHandler(rpc ScheduledTaskRPC, defaultModel string) *ScheduledTaskHandler {
	return &ScheduledTaskHandler{rpc: rpc, defaultModel: defaultModel}
}

// WithContentExistence registers the history "content deleted" annotation. It
// is optional; nil leaves the DTO without the marker.
func (h *ScheduledTaskHandler) WithContentExistence(fn func(ctx context.Context, source, sessionID string) (bool, error)) *ScheduledTaskHandler {
	h.contentExists = fn
	return h
}

type scheduledTaskCreateRequest struct {
	Name             string                       `json:"name"`
	Prompt           string                       `json:"prompt"`
	Schedule         schedule.Rule                `json:"schedule"`
	Session          model.ScheduledSessionPolicy `json:"session"`
	Model            string                       `json:"model"`
	ReasoningEffort  string                       `json:"reasoning_effort"`
	Enabled          *bool                        `json:"enabled"`
	ResourceRevision uint64                       `json:"resource_revision"`
	ManifestHash     string                       `json:"manifest_hash"`
}

type scheduledTaskUpdateRequest struct {
	Name             *string                       `json:"name"`
	Prompt           *string                       `json:"prompt"`
	Schedule         *schedule.Rule                `json:"schedule"`
	Session          *model.ScheduledSessionPolicy `json:"session"`
	Model            *string                       `json:"model"`
	ReasoningEffort  *string                       `json:"reasoning_effort"`
	Enabled          *bool                         `json:"enabled"`
	ResourceRevision *uint64                       `json:"resource_revision"`
	ManifestHash     *string                       `json:"manifest_hash"`
	ExpectedRevision *int64                        `json:"expected_revision"`
}

// scheduledTaskDTO preserves the pre-extraction JSON contract and adds the
// resource reference, revision and read-only hold fields.
type scheduledTaskDTO struct {
	TaskID                    string                       `json:"task_id"`
	Name                      string                       `json:"name"`
	Prompt                    string                       `json:"prompt"`
	Schedule                  schedule.Rule                `json:"schedule"`
	Session                   model.ScheduledSessionPolicy `json:"session"`
	Model                     string                       `json:"model,omitempty"`
	ReasoningEffort           string                       `json:"reasoning_effort,omitempty"`
	Enabled                   bool                         `json:"enabled"`
	NextRunAt                 *time.Time                   `json:"next_run_at,omitempty"`
	Revision                  int64                        `json:"revision"`
	PausedReason              string                       `json:"paused_reason,omitempty"`
	ResourceRevision          uint64                       `json:"resource_revision"`
	ManifestHash              string                       `json:"manifest_hash,omitempty"`
	ExecutionHoldOccurrenceID string                       `json:"execution_hold_occurrence_id,omitempty"`
	BlockedReason             string                       `json:"blocked_reason,omitempty"`
	UpdateTime                time.Time                    `json:"update_time"`
	CreateTime                time.Time                    `json:"create_time"`
}

type scheduledOccurrenceDTO struct {
	OccurrenceID     string          `json:"occurrence_id"`
	TaskID           string          `json:"task_id"`
	Revision         int64           `json:"revision"`
	Snapshot         json.RawMessage `json:"snapshot,omitempty"`
	ScheduledFor     time.Time       `json:"scheduled_for"`
	RunID            string          `json:"run_id"`
	SessionID        string          `json:"session_id"`
	SessionSource    string          `json:"session_source"`
	Status           string          `json:"status"`
	Reason           string          `json:"reason,omitempty"`
	AttemptCount     int             `json:"attempt_count"`
	ResourceRevision uint64          `json:"resource_revision"`
	ManifestHash     string          `json:"manifest_hash,omitempty"`
	ActualModel      string          `json:"actual_model,omitempty"`
	StartedAt        *time.Time      `json:"started_at,omitempty"`
	FinishedAt       *time.Time      `json:"finished_at,omitempty"`
	Error            string          `json:"error,omitempty"`
	ContentDeleted   bool            `json:"content_deleted,omitempty"`
	UpdateTime       time.Time       `json:"update_time"`
	CreateTime       time.Time       `json:"create_time"`
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
	if err := rejectLegacySessionPolicy(req.Session.Mode); err != nil {
		writeScheduledTaskError(c, err)
		return
	}
	resp, err := h.rpc.CreateTask(c.Request.Context(), &schedulerv1.CreateTaskRequest{
		Actor: &schedulerv1.Actor{UserId: userID},
		Config: &schedulerv1.TaskConfig{
			Name: req.Name, Prompt: req.Prompt, Schedule: scheduleProto(req.Schedule),
			Model: req.Model, ReasoningEffort: req.ReasoningEffort,
			Enabled:          req.Enabled == nil || *req.Enabled,
			ResourceRevision: req.ResourceRevision, ManifestHash: req.ManifestHash,
		},
	})
	if err != nil {
		writeScheduledTaskError(c, err)
		return
	}
	c.JSON(http.StatusCreated, h.taskDTO(resp.GetTask()))
}

func (h *ScheduledTaskHandler) Get(c *gin.Context) {
	userID, ok := scheduledTaskUser(c)
	if !ok {
		return
	}
	resp, err := h.rpc.GetTask(c.Request.Context(), &schedulerv1.GetTaskRequest{
		Actor: &schedulerv1.Actor{UserId: userID}, TaskId: c.Param("task_id"),
	})
	if err != nil {
		writeScheduledTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, h.taskDTO(resp.GetTask()))
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
	resp, err := h.rpc.ListTasks(c.Request.Context(), &schedulerv1.ListTasksRequest{
		Actor: &schedulerv1.Actor{UserId: userID}, Limit: int32(limit), Offset: int32(offset),
	})
	if err != nil {
		writeScheduledTaskError(c, err)
		return
	}
	tasks := make([]scheduledTaskDTO, 0, len(resp.GetTasks()))
	for _, t := range resp.GetTasks() {
		tasks = append(tasks, h.taskDTO(t))
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
	if req.Session != nil {
		if err := rejectLegacySessionPolicy(req.Session.Mode); err != nil {
			writeScheduledTaskError(c, err)
			return
		}
	}
	taskID := c.Param("task_id")
	current, err := h.rpc.GetTask(c.Request.Context(), &schedulerv1.GetTaskRequest{
		Actor: &schedulerv1.Actor{UserId: userID}, TaskId: taskID,
	})
	if err != nil {
		writeScheduledTaskError(c, err)
		return
	}
	cfg := mergeTaskConfig(current.GetTask().GetConfig(), req)
	expected := int64(0)
	if req.ExpectedRevision != nil {
		expected = *req.ExpectedRevision
	}
	resp, err := h.rpc.UpdateTask(c.Request.Context(), &schedulerv1.UpdateTaskRequest{
		Actor: &schedulerv1.Actor{UserId: userID}, TaskId: taskID, Config: cfg, ExpectedRevision: expected,
	})
	if err != nil {
		writeScheduledTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, h.taskDTO(resp.GetTask()))
}

func (h *ScheduledTaskHandler) Delete(c *gin.Context) {
	userID, ok := scheduledTaskUser(c)
	if !ok {
		return
	}
	_, err := h.rpc.DeleteTask(c.Request.Context(), &schedulerv1.DeleteTaskRequest{
		Actor: &schedulerv1.Actor{UserId: userID}, TaskId: c.Param("task_id"),
	})
	if err != nil {
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
	resp, err := h.rpc.ListOccurrences(c.Request.Context(), &schedulerv1.ListOccurrencesRequest{
		Actor: &schedulerv1.Actor{UserId: userID}, TaskId: c.Param("task_id"),
		Limit: int32(limit), Offset: int32(offset),
	})
	if err != nil {
		writeScheduledTaskError(c, err)
		return
	}
	runs := make([]scheduledOccurrenceDTO, 0, len(resp.GetOccurrences()))
	for _, o := range resp.GetOccurrences() {
		dto := h.occurrenceDTO(o)
		if h.contentExists != nil && dto.SessionID != "" && dto.SessionSource != "" {
			if exists, cerr := h.contentExists(c.Request.Context(), dto.SessionSource, dto.SessionID); cerr == nil && !exists {
				dto.ContentDeleted = true
			}
		}
		runs = append(runs, dto)
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs, "limit": limit, "offset": offset})
}

func (h *ScheduledTaskHandler) taskDTO(t *schedulerv1.Task) scheduledTaskDTO {
	if t == nil {
		return scheduledTaskDTO{}
	}
	cfg := t.GetConfig()
	return scheduledTaskDTO{
		TaskID: t.GetTaskId(), Name: cfg.GetName(), Prompt: cfg.GetPrompt(),
		Schedule: scheduleRule(cfg.GetSchedule()),
		Session:  model.ScheduledSessionPolicy{Mode: model.ScheduledSessionNew},
		Model:    cfg.GetModel(), ReasoningEffort: cfg.GetReasoningEffort(),
		Enabled: cfg.GetEnabled(), NextRunAt: asTimePtr(t.GetNextRunAt()), Revision: t.GetRevision(),
		PausedReason: t.GetPausedReason(), ResourceRevision: cfg.GetResourceRevision(),
		ManifestHash:              cfg.GetManifestHash(),
		ExecutionHoldOccurrenceID: t.GetExecutionHoldOccurrenceId(),
		BlockedReason:             t.GetBlockedReason(),
		UpdateTime:                asTime(t.GetUpdateTime()), CreateTime: asTime(t.GetCreateTime()),
	}
}

func (h *ScheduledTaskHandler) occurrenceDTO(o *schedulerv1.Occurrence) scheduledOccurrenceDTO {
	if o == nil {
		return scheduledOccurrenceDTO{}
	}
	dto := scheduledOccurrenceDTO{
		OccurrenceID: o.GetOccurrenceId(), TaskID: o.GetTaskId(), Revision: o.GetTaskRevision(),
		ScheduledFor: asTime(o.GetScheduledFor()), RunID: o.GetRunId(), SessionID: o.GetSessionId(),
		SessionSource: sessionSourceString(o.GetSessionSource()), Status: o.GetStatus(),
		Reason: o.GetReason(), AttemptCount: int(o.GetAttemptCount()),
		ResourceRevision: o.GetResourceRevision(), ManifestHash: o.GetManifestHash(),
		StartedAt: asTimePtr(o.GetStartedAt()), FinishedAt: asTimePtr(o.GetFinishedAt()),
		Error: o.GetLastError(),
	}
	if raw := o.GetSnapshotJson(); json.Valid([]byte(raw)) {
		dto.Snapshot = json.RawMessage(raw)
		var snap model.ScheduledTaskSnapshot
		if json.Unmarshal([]byte(raw), &snap) == nil {
			dto.ActualModel = snap.Model
			if dto.ActualModel == "" {
				dto.ActualModel = h.defaultModel
			}
		}
	}
	return dto
}

func mergeTaskConfig(current *schedulerv1.TaskConfig, req scheduledTaskUpdateRequest) *schedulerv1.TaskConfig {
	cfg := &schedulerv1.TaskConfig{
		Name: current.GetName(), Prompt: current.GetPrompt(), Schedule: current.GetSchedule(),
		Model: current.GetModel(), ReasoningEffort: current.GetReasoningEffort(),
		Enabled: current.GetEnabled(), ResourceRevision: current.GetResourceRevision(),
		ManifestHash: current.GetManifestHash(),
	}
	if req.Name != nil {
		cfg.Name = *req.Name
	}
	if req.Prompt != nil {
		cfg.Prompt = *req.Prompt
	}
	if req.Schedule != nil {
		cfg.Schedule = scheduleProto(*req.Schedule)
	}
	if req.Model != nil {
		cfg.Model = *req.Model
	}
	if req.ReasoningEffort != nil {
		cfg.ReasoningEffort = *req.ReasoningEffort
	}
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if req.ResourceRevision != nil {
		cfg.ResourceRevision = *req.ResourceRevision
	}
	if req.ManifestHash != nil {
		cfg.ManifestHash = *req.ManifestHash
	}
	return cfg
}

func rejectLegacySessionPolicy(mode string) error {
	switch mode {
	case "", model.ScheduledSessionNew:
		return nil
	case model.ScheduledSessionExisting:
		return rpcx.Errorf(rpcx.CodeInvalidArgument,
			"session.mode=existing is no longer accepted; scheduled tasks always use one dedicated session per occurrence")
	default:
		return rpcx.Errorf(rpcx.CodeInvalidArgument, "session.mode must be new")
	}
}

func scheduleProto(rule schedule.Rule) *schedulerv1.Schedule {
	out := &schedulerv1.Schedule{CronExpression: rule.Expression, Timezone: rule.Timezone}
	if rule.Type == schedule.TypeAt {
		out.Type = schedulerv1.ScheduleType_SCHEDULE_TYPE_AT
		out.RunAt = rule.RunAt
	} else {
		out.Type = schedulerv1.ScheduleType_SCHEDULE_TYPE_CRON
	}
	return out
}

func scheduleRule(p *schedulerv1.Schedule) schedule.Rule {
	rule := schedule.Rule{Expression: p.GetCronExpression(), Timezone: p.GetTimezone()}
	if p.GetType() == schedulerv1.ScheduleType_SCHEDULE_TYPE_AT {
		rule.Type = schedule.TypeAt
		rule.RunAt = p.GetRunAt()
	} else {
		rule.Type = schedule.TypeCron
	}
	return rule
}

func sessionSourceString(src schedulerv1.SessionSource) string {
	switch src {
	case schedulerv1.SessionSource_SESSION_SOURCE_SCHEDULED:
		return string(model.SessionSourceScheduled)
	case schedulerv1.SessionSource_SESSION_SOURCE_LEGACY_INTERACTIVE:
		return string(model.SessionSourceLegacyInteractive)
	default:
		return ""
	}
}

func asTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime().UTC()
}

func asTimePtr(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime().UTC()
	return &t
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
	status, code, message := rpcx.HTTPStatus(err)
	if status == 500 {
		c.JSON(status, errorBody("INTERNAL", "scheduled task operation failed"))
		return
	}
	c.JSON(status, errorBody(code, message))
}
