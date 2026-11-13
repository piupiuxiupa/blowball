package handler

import (
	"time"

	"github.com/lush/blowball/pkg/agent"
	"github.com/lush/blowball/pkg/memory"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/stream"
	turnexec "github.com/lush/blowball/pkg/turn"
)

// Transport-facing aliases keep the existing handler contract while the
// implementation lives in the neutral turn package.
type OrchestratorRunner = turnexec.OrchestratorRunner
type TurnHooks = turnexec.TurnHooks
type TurnEventTap = turnexec.TurnEventTap
type innerRunner = turnexec.InnerRunner

func NewTurnEventTap() *TurnEventTap { return turnexec.NewTurnEventTap() }

func NewOrchestratorAdapter(o *agent.Orchestrator) OrchestratorRunner {
	return turnexec.NewOrchestratorAdapter(o)
}

func newOrchestratorAdapter(inner innerRunner) OrchestratorRunner {
	return turnexec.NewOrchestratorAdapterFromRunner(inner)
}

func MergeEvents(events []stream.StreamEvent) []stream.StreamEvent {
	return turnexec.MergeEvents(events)
}

func MessagesToAgentMessages(prior []model.Message, calls []model.ToolCall) ([]agent.Message, error) {
	return turnexec.MessagesToAgentMessages(prior, calls)
}

func MessagesToAgentMessagesIndexed(prior []model.Message, calls []model.ToolCall) ([]agent.Message, []int, error) {
	return turnexec.MessagesToAgentMessagesIndexed(prior, calls)
}

func UserMessage(sessionID, traceID, content string, msgTime time.Time, attachments ...model.MessageAttachmentRef) model.Message {
	return turnexec.UserMessage(sessionID, traceID, content, msgTime, attachments...)
}

func deterministicClientMsgID(traceID string, msgIndex int) string {
	return turnexec.DeterministicClientMsgID(traceID, msgIndex)
}

func runIDFromEvent(e stream.StreamEvent) string { return turnexec.RunIDFromEvent(e) }

func agentInstanceIDFromEvent(e stream.StreamEvent) string {
	return turnexec.AgentInstanceIDFromEvent(e)
}

func topLevelAssistantText(merged []stream.StreamEvent) string {
	return turnexec.TopLevelAssistantText(merged)
}

type recallArgs = turnexec.RecallArgs
type recallInjection = turnexec.RecallInjection

const recallToolName = turnexec.RecallToolName

func newRecallInjection(sessionID, traceID, query string, res memory.RecallResult) recallInjection {
	return turnexec.NewRecallInjection(sessionID, traceID, query, res)
}

func deterministicRecallCallID(sessionID, traceID string) string {
	return turnexec.DeterministicRecallCallID(sessionID, traceID)
}

func queryPreview(query string) string { return turnexec.QueryPreview(query) }

func injectedRecallKeys(msgs []agent.Message) map[string]struct{} {
	return turnexec.InjectedRecallKeys(msgs)
}

func frameCompactedSummary(content string) string { return turnexec.FrameCompactedSummary(content) }

const summaryLeadIn = turnexec.SummaryLeadIn

func indexToolCallsByID(calls []model.ToolCall) map[string]model.ToolCall {
	return turnexec.IndexToolCallsByID(calls)
}

func stitchCompacted(rows []model.Message, calls []model.ToolCall, agentMsgs []agent.Message, rec *model.ContextCompaction) []agent.Message {
	return turnexec.StitchCompacted(rows, calls, agentMsgs, rec)
}

func applyRetainedResultSummaries(tail []agent.Message, calls []model.ToolCall, rec *model.ContextCompaction) {
	turnexec.ApplyRetainedResultSummaries(tail, calls, rec)
}

func applySummaryToRoundLive(msgs []agent.Message, rec *model.ContextCompaction) []agent.Message {
	return turnexec.ApplySummaryToRoundLive(msgs, rec)
}

type roundBatch struct {
	msgs  []model.Message
	calls []model.ToolCall
}

type roundRenderOptions struct {
	terminal    bool
	interrupted bool
}

func (p turnPersistInfo) buildRoundFinalBatch(
	merged []stream.StreamEvent,
	from int,
	userPersisted bool,
	now time.Time,
	opts roundRenderOptions,
) (roundBatch, int) {
	batch, consumed := (turnexec.PersistInfo{
		SessionID:       p.sessionID,
		UserID:          p.userID,
		TraceID:         p.traceID,
		UserContent:     p.userContent,
		UserAttachments: p.userAttachments,
		UserMsgTime:     p.userMsgTime,
	}).BuildRoundFinalBatch(merged, from, userPersisted, now, turnexec.RoundRenderOptions{
		Terminal:    opts.terminal,
		Interrupted: opts.interrupted,
	})
	return roundBatch{msgs: batch.Msgs, calls: batch.Calls}, consumed
}
