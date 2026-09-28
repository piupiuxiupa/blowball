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
// storage): the assistant final row, one tiny tool PLACEHOLDER row per call
// (the positional anchor in the messages stream), and the canonical
// tool_calls payload rows — all written in a single cross-table transaction.
type roundBatch struct {
	msgs  []model.Message
	calls []model.ToolCall
}

// roundState accumulates one LLM round while walking the merged event
// stream: the assistant's content/reasoning, the round's tool calls (results
// filled in as they arrive), and each call's merged-stream ordinal (the
// placeholder row's msg_index).
type roundState struct {
	firstOrd  int // merged-stream ordinal of the round's first event
	content   strings.Builder
	reasoning strings.Builder
	calls     []model.ToolCall
	ord       []int // merged ordinal per call (placeholder msg_index)
	callOrd   map[string]int
}

func newRoundState(firstOrd int) *roundState {
	return &roundState{firstOrd: firstOrd, callOrd: map[string]int{}}
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

// buildRoundFinalBatch renders final rows for merged[from:] plus the user row
// when from == 0 and it was not send-time persisted.
//
// A round closes when the next round's first token/reasoning event arrives —
// by then all of its tool results have landed (the agent loop awaits
// parallel results before the next LLM call). opts.terminal closes the
// trailing round unconditionally. Each closed round emits: one assistant
// final row (event_type=message, carries merged content/reasoning), then one
// PLACEHOLDER row per tool call (event_type=tool_call, carries only
// tool_call_id + name; its msg_index position anchors the interaction in the
// stream). The second return value is the merged ordinal consumed; the
// caller advances the shared cursor only on persist success, and the
// deterministic client_msg_id {trace}:{ordinal} collapses any redelivery.
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

	// emit appends r's rows unless the round predates the cursor (already
	// persisted by an earlier render).
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
			Role:             model.RoleAssistant,
			EventType:        model.EventTypeMessage,
			Status:           status,
			Content:          r.content.String(),
			ReasoningContent: r.reasoning.String(),
			TraceID:          p.traceID,
			ClientMsgID:      deterministicClientMsgID(p.traceID, r.firstOrd),
		})
		for i, c := range r.calls {
			ord := r.ord[i]
			if ord < from {
				continue // placeholder already persisted by an earlier render
			}
			batch.msgs = append(batch.msgs, model.Message{
				SessionID:   p.sessionID,
				MsgTime:     now,
				Agent:       model.AgentConfucius,
				MsgIndex:    ord,
				Role:        model.RoleAssistant,
				EventType:   model.EventTypeToolCall,
				Content:     c.Name,
				TraceID:     p.traceID,
				ClientMsgID: deterministicClientMsgID(p.traceID, ord),
				ToolCallID:  c.ToolCallID,
			})
		}
		batch.calls = append(batch.calls, r.calls...)
	}

	// Walk the whole stream so ordinals are render-independent; emit is
	// gated on firstOrd >= from.
	var cur *roundState
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
				cur = newRoundState(i)
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
				cur = newRoundState(i)
			}
			id, _ := e.Meta[stream.MetaToolCallID].(string)
			args := "{}"
			if raw, ok := e.Meta[stream.MetaArgs]; ok && raw != nil {
				args = string(marshalEventJSON(raw))
			}
			cur.callOrd[id] = len(cur.calls)
			cur.ord = append(cur.ord, i)
			cur.calls = append(cur.calls, model.ToolCall{
				SessionID:  p.sessionID,
				TurnID:     p.traceID,
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
