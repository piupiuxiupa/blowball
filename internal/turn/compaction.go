package turn

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/service"
)

func (e *Executor) newRoundHook(tap *TurnEventTap, persist PersistInfo, flushed *FlushState, window, compactionBudget int, summaryModel string) agent.RoundHook {
	return func(ctx context.Context, lastUsage agent.Usage) []agent.Message {
		contextTokens := lastUsage.PromptTokens + lastUsage.CompletionTokens
		threshold := compactionBudget
		if threshold <= 0 {
			threshold = window
		}
		if !e.compSvc.ShouldCompact(contextTokens, threshold) {
			return nil
		}
		return e.compactMidTurn(ctx, tap, persist, flushed, window, compactionBudget, summaryModel, contextTokens, false, model.CompactionTriggerMidTurn)
	}
}

func (e *Executor) compactMidTurn(ctx context.Context, tap *TurnEventTap, persist PersistInfo, flushed *FlushState, window, inputBudget int, summaryModel string, contextTokens int, measured bool, triggerKind string) []agent.Message {
	log := logger.FromContext(ctx).With(
		zap.String("op", "turn.round_hook"),
		zap.Int("context_tokens", contextTokens),
		zap.Int("input_budget_tokens", inputBudget),
		zap.String("trigger_kind", triggerKind),
	)

	events := tap.Snapshot(ctx)
	if events == nil {
		log.Warn("mid-turn event snapshot unavailable; compaction skipped")
		return nil
	}
	merged := MergeEvents(events)
	flushed.PersistMu.Lock()
	batch, consumed := persist.BuildRoundFinalBatch(merged, flushed.Count(), flushed.UserPersisted(), time.Now().UTC(),
		RoundRenderOptions{Terminal: true})
	if len(batch.Msgs) > 0 || len(batch.Calls) > 0 {
		if err := e.sessSvc.SaveFinalMessages(ctx, persist.UserID, batch.Msgs, batch.Calls); err != nil {
			flushed.PersistMu.Unlock()
			log.Warn("mid-turn flush failed; compaction skipped", zap.Error(err))
			return nil
		}
		flushed.Mark(consumed)
	}
	flushed.PersistMu.Unlock()

	rows, calls, err := e.compSvc.RecoverDurable(ctx, persist.SessionID)
	if err != nil {
		log.Warn("mid-turn durable recover failed; compaction skipped", zap.Error(err))
		return nil
	}
	agentMsgs, lastRow, err := MessagesToAgentMessagesIndexed(rows, calls)
	if err != nil {
		log.Warn("mid-turn reconstruct failed; compaction skipped", zap.Error(err))
		return nil
	}
	requestOverhead := 0
	if env, ok := agent.RequestEnvelopeFromContext(ctx); ok {
		requestOverhead = agent.EstimateRenderedRequest(env.System, nil, env.Tools).Tokens
	}
	requestOverhead += agent.EstimateCurrentStatusOverhead(time.Now())
	liveMessages := agent.LiveMessagesFromContext(ctx)

	rec, ok := e.compSvc.Compact(ctx, service.CompactionInput{
		SessionID:             persist.SessionID,
		UserID:                persist.UserID,
		TriggerKind:           triggerKind,
		TriggerTokens:         contextTokens,
		Rows:                  rows,
		AgentMsgs:             agentMsgs,
		LastRow:               lastRow,
		SummaryModel:          summaryModel,
		InputBudget:           inputBudget,
		TriggerTokensMeasured: measured,
		RequestOverheadTokens: requestOverhead,
		LiveMessages:          liveMessages,
	})
	if !ok {
		return nil
	}

	stitched := stitchCompacted(rows, calls, agentMsgs, rec)
	if stitched == nil {
		log.Warn("mid-turn stitch failed after compaction; keeping original context")
		return nil
	}
	final := agent.ApplyLiveOverlay(agentMsgs, stitched, liveMessages)
	agent.MarkLiveOverlayApplied(ctx)
	return applySummaryToRoundLive(final, rec)
}

func (e *Executor) newBudgetedRoundHook(tap *TurnEventTap, persist PersistInfo, flushed *FlushState, window, inputBudget int, summaryModel string) agent.RoundHook {
	base := e.newRoundHook(tap, persist, flushed, window, inputBudget, summaryModel)
	return func(ctx context.Context, lastUsage agent.Usage) []agent.Message {
		if force, forced := agent.ForcedCompactionFromContext(ctx); forced {
			triggerKind := model.CompactionTriggerMidTurn
			if strings.HasPrefix(force.Reason, "turn_start") {
				triggerKind = model.CompactionTriggerTurnStart
			}
			forceBudget := inputBudget
			if force.InputBudget > 0 {
				forceBudget = force.InputBudget
			}
			return e.compactMidTurn(ctx, tap, persist, flushed, window, forceBudget, summaryModel, force.EstimatedTokens, false, triggerKind)
		}
		return base(ctx, lastUsage)
	}
}
