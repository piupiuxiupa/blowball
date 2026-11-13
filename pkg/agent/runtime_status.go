package agent

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/logger"
	"github.com/lush/blowball/pkg/trace"
	"github.com/lush/blowball/pkg/prompt"
)

const (
	RuntimeStatusCurrent    = "current_status"
	RuntimeStatusToolBudget = "tool_budget"
)

// RuntimeStatusStore is the session-scoped, cache-only status log used by the
// root agent. Append failures are deliberately non-fatal to a turn.
type RuntimeStatusStore interface {
	AppendRuntimeStatus(ctx context.Context, sessionID string, status model.RuntimeStatus) error
	ListRuntimeStatus(ctx context.Context, sessionID string) ([]model.RuntimeStatus, error)
	ClearRuntimeStatus(ctx context.Context, sessionID string) error
}

type runtimeStatusStoreContextKey struct{}

// WithRuntimeStatusStore installs the per-turn cache-only status store.
func WithRuntimeStatusStore(ctx context.Context, store RuntimeStatusStore) context.Context {
	return context.WithValue(ctx, runtimeStatusStoreContextKey{}, store)
}

func runtimeStatusStoreFromContext(ctx context.Context) RuntimeStatusStore {
	if ctx == nil {
		return nil
	}
	store, _ := ctx.Value(runtimeStatusStoreContextKey{}).(RuntimeStatusStore)
	return store
}

func runtimeStatusSequence(messages []Message) int64 {
	var latest int64
	for _, message := range messages {
		if message.RuntimeStatusSeq > latest {
			latest = message.RuntimeStatusSeq
		}
	}
	return latest
}

func durableMessages(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	for _, message := range messages {
		if message.RuntimeStatus == "" {
			out = append(out, message)
		}
	}
	return out
}

func appendRootRuntimeStatus(
	ctx context.Context, status Message, seq int64, beforeUser bool, afterToolCallID string,
) {
	store := runtimeStatusStoreFromContext(ctx)
	sessionID := SessionIDFromContext(ctx)
	traceID := trace.FromContext(ctx)
	if store == nil || sessionID == "" || traceID == "" {
		return
	}
	entry := model.RuntimeStatus{
		StatusID:        fmt.Sprintf("%s:%d:%s", traceID, seq, status.RuntimeStatus),
		Kind:            status.RuntimeStatus,
		Content:         status.Content,
		Seq:             seq,
		AfterToolCallID: afterToolCallID,
	}
	if beforeUser {
		entry.BeforeClientMsgID = traceID + ":0"
	}
	if err := store.AppendRuntimeStatus(ctx, sessionID, entry); err != nil {
		logger.FromContext(ctx).Warn("runtime status cache append failed; continuing the turn",
			zap.String("op", "agent.runtime_status.append"),
			zap.String("session_id", sessionID),
			zap.String("trace_id", traceID),
			zap.String("status_id", entry.StatusID),
			zap.Error(err),
		)
	}
}

func clearRootRuntimeStatus(ctx context.Context) {
	store := runtimeStatusStoreFromContext(ctx)
	sessionID := SessionIDFromContext(ctx)
	if store == nil || sessionID == "" {
		return
	}
	if err := store.ClearRuntimeStatus(ctx, sessionID); err != nil {
		logger.FromContext(ctx).Warn("runtime status cache cleanup failed",
			zap.String("op", "agent.runtime_status.clear"),
			zap.String("session_id", sessionID),
			zap.Error(err),
		)
	}
}

func (c *Confucius) resetRuntimeStatusAfterCompaction(
	ctx context.Context, recovered []Message, tools []byte, timestamp time.Time,
	budget RequestBudget, statusSeq *int64,
) []Message {
	clearRootRuntimeStatus(ctx)
	*statusSeq = 1
	status := newCurrentStatusForRequest(c.cfg.SystemPrompt, recovered, tools, timestamp, budget)
	status.RuntimeStatusSeq = *statusSeq
	// A compacted session starts a fresh replay epoch on the next turn, so this
	// projection-local status need not be persisted to Redis.
	return withInitialCurrentStatus(recovered, status)
}

func newCurrentStatusMessage(timestamp time.Time, usage *prompt.ContextUsage) Message {
	return Message{
		Role:          "user",
		Content:       prompt.RenderCurrentStatus(timestamp, usage),
		RuntimeStatus: RuntimeStatusCurrent,
	}
}

func newCurrentStatusForRequest(
	system string, messages []Message, tools []byte, timestamp time.Time, budget RequestBudget,
) Message {
	if !budget.Valid() {
		return newCurrentStatusMessage(timestamp, nil)
	}
	measurement := Message{
		Role:          "user",
		Content:       prompt.RenderCurrentStatusMeasurement(timestamp),
		RuntimeStatus: RuntimeStatusCurrent,
	}
	candidate := withInitialCurrentStatus(messages, measurement)
	estimated := EstimateRenderedRequest(system, candidate, tools).Tokens
	return newCurrentStatusMessage(timestamp, &prompt.ContextUsage{
		EstimatedInputTokens:     estimated,
		InputBudgetTokens:        budget.InputTokens,
		EstimatedRemainingTokens: max(budget.InputTokens-estimated, 0),
	})
}

// EstimateCurrentStatusOverhead reserves one conservative current_status user
// message in compaction candidates before the fresh status is actually built.
func EstimateCurrentStatusOverhead(timestamp time.Time) int {
	measurement := Message{
		Role:          "user",
		Content:       prompt.RenderCurrentStatusMeasurement(timestamp),
		RuntimeStatus: RuntimeStatusCurrent,
	}
	return EstimateRenderedRequest("", []Message{measurement}, nil).Tokens
}

func newToolBudgetMessage(roundsUsed, maxRounds int) Message {
	return Message{
		Role:          "user",
		Content:       prompt.RenderToolBudget(roundsUsed, maxRounds),
		RuntimeStatus: RuntimeStatusToolBudget,
	}
}

func withInitialCurrentStatus(messages []Message, status Message) []Message {
	out := make([]Message, 0, len(messages)+1)
	if len(messages) > 0 && messages[len(messages)-1].Role == "user" {
		out = append(out, messages[:len(messages)-1]...)
		out = append(out, status, messages[len(messages)-1])
		return out
	}
	out = append(out, messages...)
	return append(out, status)
}
