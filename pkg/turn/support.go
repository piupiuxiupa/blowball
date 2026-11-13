package turn

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/lush/blowball/pkg/agent"
	"github.com/lush/blowball/pkg/memory"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/stream"
)

// PersistInfo bundles the per-turn identifiers used to render durable rows.
type PersistInfo struct {
	SessionID       string
	UserID          string
	TraceID         string
	UserContent     string
	UserAttachments []model.MessageAttachmentRef
	UserMsgTime     time.Time
}

// FlushState tracks the merged-event prefix already persisted for a turn.
type FlushState struct {
	mu        sync.Mutex
	flushed   int
	userRow   bool
	PersistMu sync.Mutex
}

func (f *FlushState) Mark(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n > f.flushed {
		f.flushed = n
	}
}

func (f *FlushState) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushed
}

func (f *FlushState) MarkUserPersisted() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userRow = true
}

func (f *FlushState) UserPersisted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.userRow
}

func configuredInputBudget(window, completion int) int {
	if window <= 0 || completion <= 0 {
		return 0
	}
	return agent.NewRequestBudget(window, completion).InputTokens
}

// BuildTurnUsage materializes the durable usage row from the done event.
func BuildTurnUsage(sessionID, traceID, userID, turnModel string, usage map[string]any) (model.TurnUsage, bool) {
	if len(usage) == 0 {
		return model.TurnUsage{}, false
	}
	raw, err := json.Marshal(usage)
	if err != nil {
		return model.TurnUsage{}, false
	}
	return model.TurnUsage{
		SessionID:     sessionID,
		TraceID:       traceID,
		UserID:        userID,
		Model:         turnModel,
		UsageJSON:     string(raw),
		TotalTokens:   ExtractTotalTokens(usage),
		ContextTokens: ExtractContextTokens(usage),
	}, true
}

func ExtractTotalTokens(usage map[string]any) int {
	total, ok := usage["total"].(map[string]any)
	if !ok {
		return 0
	}
	return numericTokenValue(total["total_tokens"])
}

func ExtractContextTokens(usage map[string]any) int {
	meta, ok := usage["meta"].(map[string]any)
	if !ok {
		return 0
	}
	return numericTokenValue(meta["context_tokens"])
}

func numericTokenValue(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func DeterministicClientMsgID(traceID string, msgIndex int) string {
	return deterministicClientMsgID(traceID, msgIndex)
}

func RunIDFromEvent(e stream.StreamEvent) string { return runIDFromEvent(e) }

func AgentInstanceIDFromEvent(e stream.StreamEvent) string { return agentInstanceIDFromEvent(e) }

func TopLevelAssistantText(merged []stream.StreamEvent) string { return topLevelAssistantText(merged) }

const RecallToolName = recallToolName

func DeterministicRecallCallID(sessionID, traceID string) string {
	return deterministicRecallCallID(sessionID, traceID)
}

func QueryPreview(query string) string { return queryPreview(query) }

func FrameCompactedSummary(content string) string { return frameCompactedSummary(content) }

const SummaryLeadIn = summaryLeadIn

func IndexToolCallsByID(calls []model.ToolCall) map[string]model.ToolCall {
	return indexToolCallsByID(calls)
}

func StitchCompacted(rows []model.Message, calls []model.ToolCall, agentMsgs []agent.Message, rec *model.ContextCompaction) []agent.Message {
	return stitchCompacted(rows, calls, agentMsgs, rec)
}

func ApplySummaryToRoundLive(msgs []agent.Message, rec *model.ContextCompaction) []agent.Message {
	return applySummaryToRoundLive(msgs, rec)
}

func ApplyRetainedResultSummaries(tail []agent.Message, calls []model.ToolCall, rec *model.ContextCompaction) {
	applyRetainedResultSummaries(tail, calls, rec)
}

// AttachmentFilenames projects attachment references onto display names.
func AttachmentFilenames(refs []model.MessageAttachmentRef) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Filename
	}
	return out
}

// RenderAttachmentPlaceholders is the stable history/missing-file rendering.
func RenderAttachmentPlaceholders(content string, filenames []string) string {
	if len(filenames) == 0 {
		return content
	}
	var b strings.Builder
	b.WriteString(content)
	for _, f := range filenames {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("[图片: " + f + "]")
	}
	return b.String()
}

func attachmentFilenames(refs []model.MessageAttachmentRef) []string {
	return AttachmentFilenames(refs)
}

func renderAttachmentPlaceholders(content string, filenames []string) string {
	return RenderAttachmentPlaceholders(content, filenames)
}

// RecallResult is re-exported through helper signatures without widening the
// memory package's public surface.
type RecallResult = memory.RecallResult
