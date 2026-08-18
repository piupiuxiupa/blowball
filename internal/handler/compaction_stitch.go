package handler

import (
	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
)

// summaryLeadIn is the fixed preamble framing a compacted checkpoint
// (context-compaction capability, design D9). English engineering prose
// regardless of the conversation language — same choice as the checkpoint
// template itself — because it is an instruction to the model, not content.
const summaryLeadIn = "This is an automatically generated checkpoint condensing an earlier span of the conversation. Treat it as authoritative context for everything below it."

// frameCompactedSummary wraps the bare checkpoint record content in the
// stable <compacted-summary> wire frame. The frame is applied at stitch time
// (not stored) so the record stays framing-agnostic.
func frameCompactedSummary(content string) string {
	return summaryLeadIn + "\n\n<compacted-summary>\n" + content + "\n</compacted-summary>"
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
func stitchCompacted(rows []model.Message, agentMsgs []agent.Message, rec *model.ContextCompaction) []agent.Message {
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
	tailMsgs, err := MessagesToAgentMessages(rows[ord+1:])
	if err != nil {
		return nil
	}
	out := make([]agent.Message, 0, 2+len(tailMsgs))
	out = append(out, agentMsgs[0])
	out = append(out, agent.Message{Role: "user", Content: frameCompactedSummary(rec.Content)})
	out = append(out, tailMsgs...)
	return out
}
