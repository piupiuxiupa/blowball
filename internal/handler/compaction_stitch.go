package handler

import (
	"encoding/json"
	"strings"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
)

// summaryLeadIn is the fixed preamble framing a compacted checkpoint
// (context-compaction capability, design D9). English engineering prose
// regardless of the conversation language — same choice as the checkpoint
// template itself — because it is an instruction to the model, not content.
const summaryLeadIn = agent.CompactedSummaryLeadIn

// frameCompactedSummary wraps the bare checkpoint record content in the
// stable <compacted-summary> wire frame. The frame is applied at stitch time
// (not stored) so the record stays framing-agnostic.
func frameCompactedSummary(content string) string {
	return agent.FrameCompactedSummary(content)
}

// stitchCompacted builds the model context for a compacted session:
//
//	[first agent message verbatim] + [framed summary as one user-role
//	message] + [every message reconstructed from rows after the boundary]
//
// The first message is the session's original task anchor; the summary rides
// the user role (the model's best-established instruction-compliance); the
// tail is reconstructed in isolation from the post-boundary rows, which is
// exactly the retained tail the compaction selected (boundaries always fall
// on pairing-unit edges, so isolated reconstruction equals the full-history
// slice). Returns nil whenever the boundary row cannot be located or the tail
// fails to reconstruct — the caller's contract is to degrade to the full
// history, never to error the turn.
func stitchCompacted(rows []model.Message, calls []model.ToolCall, agentMsgs []agent.Message, rec *model.ContextCompaction) []agent.Message {
	if rec == nil || len(agentMsgs) == 0 {
		return nil
	}
	ord := -1
	for i := range rows {
		if rows[i].ID == rec.BoundaryMsgID &&
			rows[i].MsgIndex == rec.BoundaryMsgIndex &&
			rows[i].MsgTime.Equal(rec.BoundaryMsgTime) {
			ord = i
			break
		}
	}
	if ord < 0 {
		return nil
	}
	// The full calls list is safe here: tail rows join their calls by
	// (trace, round); calls of compacted turns match no tail row and drop out.
	tailMsgs, err := MessagesToAgentMessages(rows[ord+1:], calls)
	if err != nil {
		return nil
	}
	out := make([]agent.Message, 0, 2+len(tailMsgs))
	anchor := agentMsgs[0]
	if rec.AnchorTruncated && strings.TrimSpace(rec.AnchorJSON) != "" {
		var projected agent.Message
		if err := json.Unmarshal([]byte(rec.AnchorJSON), &projected); err == nil {
			anchor = projected
		}
	}
	out = append(out, anchor)
	out = append(out, agent.Message{Role: "user", Content: frameCompactedSummary(rec.Content)})
	applyRetainedResultSummaries(tailMsgs, calls, rec)
	out = append(out, tailMsgs...)
	return out
}

// applyRetainedResultSummaries replaces retained role=tool bodies with the
// deterministic summaries persisted by the validated compaction plan. Calls
// not present in the record (legacy record or a newly appended pair) keep
// their reconstructed body. Applying the same record repeatedly is stable.
func applyRetainedResultSummaries(tail []agent.Message, calls []model.ToolCall, rec *model.ContextCompaction) {
	if rec == nil || len(rec.RetainedResults) == 0 {
		return
	}
	for i := range tail {
		if tail[i].Role != "tool" || tail[i].ToolCallID == "" {
			continue
		}
		if summary, ok := rec.RetainedResults[tail[i].ToolCallID]; ok && strings.TrimSpace(summary) != "" {
			tail[i].Content = summary
		}
	}
}

// applySummaryToRoundLive applies retained-result summaries to the in-memory
// round immediately after mid-turn compaction. This is the same projection the
// persistence/recovery path applies, so the same-turn continuation cannot
// resend an oversized raw tool body.
func applySummaryToRoundLive(msgs []agent.Message, rec *model.ContextCompaction) []agent.Message {
	if rec == nil || len(rec.RetainedResults) == 0 {
		return msgs
	}
	out := make([]agent.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if out[i].Role != "tool" || out[i].ToolCallID == "" {
			continue
		}
		if summary, ok := rec.RetainedResults[out[i].ToolCallID]; ok && strings.TrimSpace(summary) != "" {
			out[i].Content = summary
		}
	}
	return out
}
