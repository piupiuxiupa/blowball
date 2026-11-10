package turn

import (
	"context"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
)

// RuntimeStatusStore is the cache-only root status log. Implementations must
// keep entries immutable and preserve append order.
type RuntimeStatusStore interface {
	AppendRuntimeStatus(ctx context.Context, sessionID string, status model.RuntimeStatus) error
	ListRuntimeStatus(ctx context.Context, sessionID string) ([]model.RuntimeStatus, error)
	ClearRuntimeStatus(ctx context.Context, sessionID string) error
}

// SetRuntimeStatusStore enables cache-only status replay for a turn executor.
func (e *Executor) SetRuntimeStatusStore(store RuntimeStatusStore) { e.runtimeStatus = store }

func (e *Executor) replaySessionRuntimeStatus(
	ctx context.Context, sessionID string, messages []agent.Message, lastRow []int, rows []model.Message,
) []agent.Message {
	if e.runtimeStatus == nil {
		return messages
	}
	statuses, err := e.runtimeStatus.ListRuntimeStatus(ctx, sessionID)
	if err == nil {
		if replayed, ok := replayRuntimeStatus(messages, lastRow, rows, statuses); ok {
			return replayed
		}
	}
	logger.FromContext(ctx).Warn("runtime status replay skipped; starting a new cache epoch",
		zap.String("session_id", sessionID), zap.Error(err))
	e.clearSessionRuntimeStatus(ctx, sessionID)
	return messages
}

func (e *Executor) clearSessionRuntimeStatus(ctx context.Context, sessionID string) {
	if e.runtimeStatus == nil {
		return
	}
	if err := e.runtimeStatus.ClearRuntimeStatus(ctx, sessionID); err != nil {
		logger.FromContext(ctx).Warn("runtime status cache cleanup failed",
			zap.String("session_id", sessionID), zap.Error(err))
	}
}

func replayRuntimeStatus(
	messages []agent.Message, lastRow []int, rows []model.Message, statuses []model.RuntimeStatus,
) ([]agent.Message, bool) {
	if len(statuses) == 0 {
		return append([]agent.Message(nil), messages...), true
	}
	if len(messages) != len(lastRow) {
		return nil, false
	}

	before := make(map[string][]model.RuntimeStatus)
	after := make(map[string][]model.RuntimeStatus)
	seen := make(map[string]struct{}, len(statuses))
	var previousSeq int64
	for _, status := range statuses {
		if status.StatusID == "" || status.Content == "" || status.Seq <= previousSeq {
			return nil, false
		}
		if status.Kind != agent.RuntimeStatusCurrent && status.Kind != agent.RuntimeStatusToolBudget {
			return nil, false
		}
		if _, exists := seen[status.StatusID]; exists {
			return nil, false
		}
		seen[status.StatusID] = struct{}{}
		previousSeq = status.Seq

		hasBefore := status.BeforeClientMsgID != ""
		hasAfter := status.AfterToolCallID != ""
		if hasBefore == hasAfter {
			return nil, false
		}
		if hasBefore {
			before[status.BeforeClientMsgID] = append(before[status.BeforeClientMsgID], status)
		} else {
			after[status.AfterToolCallID] = append(after[status.AfterToolCallID], status)
		}
	}

	out := make([]agent.Message, 0, len(messages)+len(statuses))
	inserted := 0
	for i, message := range messages {
		rowIndex := lastRow[i]
		if rowIndex >= 0 && rowIndex < len(rows) {
			clientMsgID := rows[rowIndex].ClientMsgID
			for _, status := range before[clientMsgID] {
				out = append(out, runtimeStatusMessage(status))
				inserted++
			}
		}
		out = append(out, message)
		if message.Role == "tool" {
			for _, status := range after[message.ToolCallID] {
				out = append(out, runtimeStatusMessage(status))
				inserted++
			}
		}
	}
	if inserted != len(statuses) {
		return nil, false
	}
	return out, true
}

func runtimeStatusMessage(status model.RuntimeStatus) agent.Message {
	return agent.Message{
		Role: "user", Content: status.Content,
		RuntimeStatus: status.Kind, RuntimeStatusSeq: status.Seq,
	}
}
