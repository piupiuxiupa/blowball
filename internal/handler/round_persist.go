package handler

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

// roundBatch is one round-boundary persistence unit (refactor-message-
// storage): the assistant final rows plus their tool interactions, written in
// a single cross-table transaction.
type roundBatch struct {
	msgs  []model.Message
	calls []model.ToolCall
}

// roundState accumulates one LLM round while walking the merged event
// stream: the assistant's content/reasoning and the round's tool calls
// (results filled in as they arrive).
type roundState struct {
	no        int
	firstOrd  int // merged-stream ordinal of the round's first event
	content   strings.Builder
	reasoning strings.Builder
	calls     []model.ToolCall
	callOrd   map[string]int // tool_call_id -> index into calls
}

func newRoundState(no, firstOrd int) *roundState {
	return &roundState{no: no, firstOrd: firstOrd, callOrd: map[string]int{}}
}

// roundRenderOptions selects what the renderer closes.
type roundRenderOptions struct {
	// terminal closes the final round even without a following-round token
	// (turn end, or the compaction hook flushing at a round boundary).
	terminal bool
	// interrupted marks the LAST emitted assistant row as truncated
	// (cancel / provider-error path).
	interrupted bool
}

// buildRoundFinalBatch renders final-content rows for merged[from:] plus the
// user row when from == 0 and it was not send-time persisted. Round ordinals
// are counted over the WHOLE merged stream so successive renders (incremental
// tick, compaction flush, turn-end suffix) number rounds identically.
//
// A round closes when the next round's first token/reasoning event arrives —
// by then all of its tool results have necessarily landed (the agent loop
// awaits parallel results before the next LLM call). opts.terminal closes the
// trailing round unconditionally. The second return value is the merged
// ordinal consumed; the caller advances the shared cursor only on persist
// success, and the deterministic client_msg_id {trace}:{firstOrd} collapses
// any redelivery.
func (p turnPersistInfo) buildRoundFinalBatch(
	merged []stream.StreamEvent,
	from int,
	userPersisted bool,
	now time.Time,
	opts roundRenderOptions,
) (roundBatch, int) {
	var batch roundBatch
	consumed := from

	if from > len(merged) {
		from = len(merged)
	}
	if from == 0 && !userPersisted {
		batch.msgs = append(batch.msgs, UserMessage(p.sessionID, p.traceID, p.userContent, p.userMsgTime))
	}

	// emit appends r's final rows unless the round predates the cursor
	// (already persisted by an earlier render).
	emit := func(r *roundState, last bool) {
		if r == nil || r.firstOrd < from {
			return
		}
		status := ""
		if opts.interrupted && last {
			status = model.MessageStatusInterrupted
		}
		batch.msgs = append(batch.msgs, model.Message{
			SessionID:        p.sessionID,
			MsgTime:          now,
			Agent:            model.AgentConfucius,
			MsgIndex:         r.firstOrd,
			RoundNo:          r.no,
			Role:             model.RoleAssistant,
			EventType:        model.EventTypeMessage,
			Status:           status,
			Content:          r.content.String(),
			ReasoningContent: r.reasoning.String(),
			TraceID:          p.traceID,
			ClientMsgID:      deterministicClientMsgID(p.traceID, r.firstOrd),
		})
		batch.calls = append(batch.calls, r.calls...)
	}

	// Walk the whole stream so round numbering is render-independent; emit
	// is gated on firstOrd >= from.
	var cur *roundState
	roundNo := 0
	afterResult := false

	for i := range merged {
		e := merged[i]
		if agentInstanceIDFromEvent(e) != "" {
			continue // sub-agent split: never persisted to messages/tool_calls
		}

		switch e.Type {
		case stream.EventToken, stream.EventReasoning:
			if cur != nil && afterResult {
				// Round boundary: everything before this ordinal belongs to
				// the now-complete round.
				emit(cur, false)
				consumed = i
				cur = nil
				afterResult = false
			}
			if cur == nil {
				roundNo++
				cur = newRoundState(roundNo, i)
			}
			if e.Type == stream.EventToken {
				cur.content.WriteString(e.Content)
			} else {
				cur.reasoning.WriteString(e.Content)
			}

		case stream.EventToolCall:
			if cur == nil {
				// Defensive: a call with no preceding narration still owns a
				// round (models may open with tool_calls directly).
				roundNo++
				cur = newRoundState(roundNo, i)
			}
			id, _ := e.Meta[stream.MetaToolCallID].(string)
			args := "{}"
			if raw, ok := e.Meta[stream.MetaArgs]; ok && raw != nil {
				args = string(marshalEventJSON(raw))
			}
			cur.callOrd[id] = len(cur.calls)
			cur.calls = append(cur.calls, model.ToolCall{
				SessionID:  p.sessionID,
				TurnID:     p.traceID,
				RoundNo:    cur.no,
				ToolCallID: id,
				Name:       e.Content,
				Arguments:  args,
				Status:     model.ToolCallStatusPending,
			})

		case stream.EventToolResult:
			id, _ := e.Meta[stream.MetaToolCallID].(string)
			if cur != nil {
				if idx, ok := cur.callOrd[id]; ok {
					cur.calls[idx].Result = e.Content
					cur.calls[idx].Status = model.ToolCallStatusCompleted
				}
			}
			afterResult = true
		}
	}

	if opts.terminal {
		if cur != nil {
			emit(cur, true)
		}
		// Terminal renders (compaction flush, turn end) close everything.
		consumed = len(merged)
	}

	return batch, consumed
}

// marshalEventJSON renders a Meta value (json.RawMessage, []byte, string, or
// map) as compact JSON bytes for storage.
func marshalEventJSON(v any) []byte {
	switch t := v.(type) {
	case []byte:
		return t
	case json.RawMessage:
		return t
	case string:
		return []byte(strconv.Quote(t))
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return []byte("{}")
		}
		return b
	}
}
