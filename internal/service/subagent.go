package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
)

// ErrSubAgentNotFound collapses missing sessions, cross-user access, missing
// instances, active runs, and mismatched run identities into one API-safe 404.
var ErrSubAgentNotFound = errors.New("sub-agent target not found")

// SubAgentRunStore is the read-only MySQL surface needed by transcript APIs.
type SubAgentRunStore interface {
	GetSessionByID(ctx context.Context, sessionID string) (*model.Session, error)
	GetSubAgentInstance(ctx context.Context, sessionID, instanceID string) (model.SubAgentInstance, bool, error)
	GetSubAgentRun(ctx context.Context, sessionID, instanceID, runID string) (model.SubAgentRun, bool, error)
	ListSubAgentRuns(ctx context.Context, sessionID, instanceID string) ([]model.SubAgentRun, error)
}

// SubAgentService turns persisted model-context deltas into stable, non-leaky
// frontend transcript DTOs.
type SubAgentService struct {
	store SubAgentRunStore
}

// NewSubAgentService wires the read model over the persistent sub-agent store.
func NewSubAgentService(store SubAgentRunStore) *SubAgentService {
	return &SubAgentService{store: store}
}

// SubAgentToolCall is the public tool-call shape embedded in an assistant item.
type SubAgentToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// SubAgentTranscriptItem is one deliberately exposed frontend transcript item.
type SubAgentTranscriptItem struct {
	Type             string             `json:"type"`
	Content          string             `json:"content,omitempty"`
	ReasoningContent string             `json:"reasoning_content,omitempty"`
	ToolCalls        []SubAgentToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string             `json:"tool_call_id,omitempty"`
	Name             string             `json:"name,omitempty"`
}

// SubAgentRunSummary is the thin metadata returned by the run-list endpoint.
type SubAgentRunSummary struct {
	AgentInstanceID string `json:"agent_instance_id"`
	RunID           string `json:"run_id"`
	PreviousRunID   string `json:"previous_run_id,omitempty"`
	RunNo           int    `json:"run_no"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	MessageCount    int    `json:"message_count"`
	StartedAt       string `json:"started_at"`
	FinishedAt      string `json:"finished_at"`
}

// SubAgentRunDetail is a single run's sanitized transcript.
type SubAgentRunDetail struct {
	AgentInstanceID string                   `json:"agent_instance_id"`
	RunID           string                   `json:"run_id"`
	PreviousRunID   string                   `json:"previous_run_id,omitempty"`
	RunNo           int                      `json:"run_no"`
	Name            string                   `json:"name"`
	Status          string                   `json:"status"`
	MessageCount    int                      `json:"message_count"`
	StartedAt       string                   `json:"started_at"`
	FinishedAt      string                   `json:"finished_at"`
	Transcript      []SubAgentTranscriptItem `json:"transcript"`
}

// ListRuns returns metadata for an owned instance's terminal runs.
func (s *SubAgentService) ListRuns(ctx context.Context, userID, sessionID, instanceID string) ([]SubAgentRunSummary, error) {
	log := subAgentLog(ctx, "subagent.list_runs", sessionID, instanceID, "")
	instance, runs, err := s.ownedRuns(ctx, userID, sessionID, instanceID)
	if err != nil {
		return nil, err
	}
	out := make([]SubAgentRunSummary, 0, len(runs))
	for _, run := range runs {
		out = append(out, subAgentRunSummary(run, instance.Name))
	}
	log.Debug("sub-agent runs listed", zap.Int("runs", len(out)))
	return out, nil
}

// GetRun returns one owned terminal run as a sanitized transcript. A legacy
// full-snapshot row intentionally renders its retained baseline; newer delta
// rows render only that run's messages.
func (s *SubAgentService) GetRun(ctx context.Context, userID, sessionID, instanceID, runID string) (SubAgentRunDetail, error) {
	log := subAgentLog(ctx, "subagent.get_run", sessionID, instanceID, runID)
	if err := s.requireOwnedInstance(ctx, userID, sessionID, instanceID); err != nil {
		return SubAgentRunDetail{}, err
	}
	run, ok, err := s.store.GetSubAgentRun(ctx, sessionID, instanceID, runID)
	if err != nil {
		log.Error("sub-agent run lookup failed", zap.Error(err))
		return SubAgentRunDetail{}, fmt.Errorf("subagent.get_run: %w", err)
	}
	if !ok {
		return SubAgentRunDetail{}, ErrSubAgentNotFound
	}
	transcript, err := subAgentTranscript(run.MessagesJSON)
	if err != nil {
		log.Error("sub-agent transcript decode failed", zap.Error(err))
		return SubAgentRunDetail{}, fmt.Errorf("subagent.get_run: decode: %w", err)
	}
	detail := SubAgentRunDetail{
		AgentInstanceID: run.AgentInstanceID,
		RunID:           run.RunID,
		PreviousRunID:   run.PreviousRunID,
		RunNo:           run.RunNo,
		Name:            run.Name,
		Status:          run.Status,
		MessageCount:    run.MessageCount,
		StartedAt:       run.StartedAt.UTC().Format(time.RFC3339Nano),
		FinishedAt:      run.FinishedAt.UTC().Format(time.RFC3339Nano),
		Transcript:      transcript,
	}
	log.Debug("sub-agent run detail returned", zap.Int("items", len(transcript)))
	return detail, nil
}

func (s *SubAgentService) requireOwnedInstance(ctx context.Context, userID, sessionID, instanceID string) error {
	sess, err := s.store.GetSessionByID(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("subagent.session: %w", err)
	}
	if sess == nil || sess.UserID != userID {
		return ErrSubAgentNotFound
	}
	_, ok, err := s.store.GetSubAgentInstance(ctx, sessionID, instanceID)
	if err != nil {
		return fmt.Errorf("subagent.instance: %w", err)
	}
	if !ok {
		return ErrSubAgentNotFound
	}
	return nil
}

func (s *SubAgentService) ownedRuns(ctx context.Context, userID, sessionID, instanceID string) (model.SubAgentInstance, []model.SubAgentRun, error) {
	sess, err := s.store.GetSessionByID(ctx, sessionID)
	if err != nil {
		return model.SubAgentInstance{}, nil, fmt.Errorf("subagent.session: %w", err)
	}
	if sess == nil || sess.UserID != userID {
		return model.SubAgentInstance{}, nil, ErrSubAgentNotFound
	}
	instance, ok, err := s.store.GetSubAgentInstance(ctx, sessionID, instanceID)
	if err != nil {
		return model.SubAgentInstance{}, nil, fmt.Errorf("subagent.instance: %w", err)
	}
	if !ok {
		return model.SubAgentInstance{}, nil, ErrSubAgentNotFound
	}
	runs, err := s.store.ListSubAgentRuns(ctx, sessionID, instanceID)
	if err != nil {
		return model.SubAgentInstance{}, nil, fmt.Errorf("subagent.list_runs: %w", err)
	}
	return instance, runs, nil
}

type subAgentWireMessage struct {
	Role             string             `json:"role"`
	Content          string             `json:"content"`
	ReasoningContent string             `json:"reasoning_content"`
	ToolCalls        []subAgentWireCall `json:"tool_calls"`
	ToolCallID       string             `json:"tool_call_id"`
	Name             string             `json:"name"`
}

type subAgentWireCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func subAgentTranscript(raw []byte) ([]SubAgentTranscriptItem, error) {
	var messages []subAgentWireMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return nil, err
	}
	out := make([]SubAgentTranscriptItem, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case "user":
			out = append(out, SubAgentTranscriptItem{Type: "task", Content: msg.Content})
		case "assistant":
			item := SubAgentTranscriptItem{
				Type: "assistant", Content: msg.Content,
				ReasoningContent: msg.ReasoningContent,
			}
			for _, call := range msg.ToolCalls {
				args := json.RawMessage(call.Function.Arguments)
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				} else if !json.Valid(args) {
					quoted, encodeErr := json.Marshal(call.Function.Arguments)
					if encodeErr != nil {
						return nil, encodeErr
					}
					args = json.RawMessage(`{"raw":` + string(quoted) + `}`)
				}
				item.ToolCalls = append(item.ToolCalls, SubAgentToolCall{
					ID: call.ID, Name: call.Function.Name, Arguments: args,
				})
			}
			out = append(out, item)
		case "tool":
			out = append(out, SubAgentTranscriptItem{
				Type: "tool_result", Content: msg.Content,
				ToolCallID: msg.ToolCallID, Name: msg.Name,
			})
		case "system":
			// Internal prompts are intentionally omitted.
		default:
			// Unknown future roles are ignored rather than leaked verbatim.
		}
	}
	return out, nil
}

func subAgentRunSummary(run model.SubAgentRun, fallbackName string) SubAgentRunSummary {
	name := run.Name
	if name == "" {
		name = fallbackName
	}
	return SubAgentRunSummary{
		AgentInstanceID: run.AgentInstanceID,
		RunID:           run.RunID,
		PreviousRunID:   run.PreviousRunID,
		RunNo:           run.RunNo,
		Name:            name,
		Status:          run.Status,
		MessageCount:    run.MessageCount,
		StartedAt:       run.StartedAt.UTC().Format(time.RFC3339Nano),
		FinishedAt:      run.FinishedAt.UTC().Format(time.RFC3339Nano),
	}
}

func subAgentLog(ctx context.Context, op, sessionID, instanceID, runID string) *zap.Logger {
	log := logger.FromContext(ctx).With(
		zap.String("op", op),
		zap.String("session_id", sessionID),
		zap.String("agent_instance_id", instanceID),
	)
	if runID != "" {
		log = log.With(zap.String("run_id", runID))
	}
	if tid := trace.FromContext(ctx); tid != "" {
		log = log.With(zap.String("trace_id", tid))
	}
	return log
}
