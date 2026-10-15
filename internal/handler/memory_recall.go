package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/memory"
	"github.com/lush/blowball/internal/model"
)

// recallToolName is the name carried by the synthetic memory-recall tool
// pair (memory-recall-cache-stable). It is intentionally NOT registered in
// any agent's callable tool list: the pair is a transcript artifact only —
// persisted at turn start, replayed byte-identically by history recovery —
// so turn-over-turn payloads stay append-only and the LLM prefix cache
// covers prior turns. The wire shape is the standard assistant tool_calls +
// tool result unit every OpenAI-compatible gateway already accepts in
// history.
const recallToolName = "recall_memories"

// recallQueryPreviewRunes caps the query preview embedded in the synthetic
// call's arguments. The real user message follows the pair immediately, so
// the preview exists only for debuggability and the transcript card — never
// duplicate the full query.
const recallQueryPreviewRunes = 160

// recallArgs is the deterministic JSON object carried in the synthetic
// call's arguments. entries is the machine-readable injected-key set the
// next turn derives session-scoped dedupe from — no separately stored
// session state.
type recallArgs struct {
	Query   string   `json:"query"`
	Entries []string `json:"entries"`
}

// recallInjection is one turn's synthetic recall tool pair, built at turn
// start (memory-recall-cache-stable): the in-memory agent messages sent to
// the orchestrator, plus everything needed to persist the pair through the
// ordinary message/tool-call rows so reconstruction replays it verbatim.
type recallInjection struct {
	callID string
	args   string // marshaled recallArgs — byte-stable across send and replay
	frame  string // rendered <user-memories> block (the tool result content)
}

// newRecallInjection derives the pair's deterministic identity from the
// session and turn trace: the same (session, turn) always mints the same
// tool_call_id, so at-least-once redelivery of the send-time batch
// collapses onto one row pair (tool_calls UK + client_msg_id).
func newRecallInjection(sessionID, traceID, query string, res memory.RecallResult) recallInjection {
	keys := make([]string, len(res.Entries))
	for i, e := range res.Entries {
		keys[i] = e.Key
	}
	args, err := json.Marshal(recallArgs{Query: queryPreview(query), Entries: keys})
	if err != nil {
		// Marshal of a plain struct with string fields cannot fail; keep the
		// defensive fallback simple and deterministic.
		args = []byte(`{"entries":[]}`)
	}
	return recallInjection{
		callID: deterministicRecallCallID(sessionID, traceID),
		args:   string(args),
		frame:  res.Frame,
	}
}

// agentMessages returns the pair exactly as history reconstruction emits it
// (flushToolCalls): one assistant message carrying the call, one tool
// message carrying the frame. Keeping the shapes byte-identical is the
// append-only contract — the next turn replays what this turn sent.
func (r recallInjection) agentMessages() []agent.Message {
	return []agent.Message{
		{
			Role: "assistant",
			ToolCalls: []agent.ToolCall{{
				ID: r.callID,
				Function: agent.ToolCallFunction{
					Name:      recallToolName,
					Arguments: r.args,
				},
			}},
		},
		{
			Role:       "tool",
			ToolCallID: r.callID,
			Name:       recallToolName,
			Content:    r.frame,
		},
	}
}

// persistRows renders the durable pair: one tool-call PLACEHOLDER row (the
// ordering anchor reconstruction joins on) plus the canonical tool_calls
// row. msg_index -1 sorts the placeholder strictly before the turn's user
// row (msg_index 0) under the shared msg_time — the (msg_time, msg_index)
// stream order then matches the payload order: pair, user message, assistant
// rows. The client_msg_id {trace}:-1 is unique per turn and idempotent on
// redelivery.
func (r recallInjection) persistRows(sessionID, traceID string, ts time.Time) (model.Message, model.ToolCall) {
	placeholder := model.Message{
		SessionID:   sessionID,
		MsgTime:     ts,
		Agent:       model.AgentConfucius,
		MsgIndex:    -1,
		Role:        model.RoleAssistant,
		EventType:   model.EventTypeToolCall,
		Content:     recallToolName,
		TraceID:     traceID,
		ClientMsgID: deterministicClientMsgID(traceID, -1),
		ToolCallID:  r.callID,
	}
	call := model.ToolCall{
		SessionID:  sessionID,
		TurnID:     traceID,
		ToolCallID: r.callID,
		Name:       recallToolName,
		Arguments:  r.args,
		Result:     r.frame,
		Status:     model.ToolCallStatusCompleted,
	}
	return placeholder, call
}

// deterministicRecallCallID mints the synthetic call's pairing key:
// "recall-" + 24 hex chars of SHA-256(session, turn). Distinct turns mint
// distinct ids; a redelivered send-time batch reuses the same one, which
// the tool_calls unique key collapses.
func deterministicRecallCallID(sessionID, traceID string) string {
	sum := sha256.Sum256([]byte("blowball-recall:" + sessionID + ":" + traceID))
	return "recall-" + hex.EncodeToString(sum[:12])
}

// queryPreview truncates the recall query rune-safely for the arguments
// object.
func queryPreview(q string) string {
	if utf8.RuneCountInString(q) <= recallQueryPreviewRunes {
		return q
	}
	r := []rune(q)
	return string(r[:recallQueryPreviewRunes]) + "…"
}

// injectedRecallKeys derives the session's already-injected memory key set
// from the recovered (possibly compaction-stitched) agent history: every
// historical recall_memories call contributes its arguments' entries list.
// Deriving from the transcript itself keeps the state restart-safe with the
// history's own durability — and naturally shrinks after compaction
// summarizes old pairs away (bounded re-injection; the prefix is broken at
// the compaction boundary regardless).
func injectedRecallKeys(msgs []agent.Message) map[string]struct{} {
	var set map[string]struct{}
	for _, m := range msgs {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name != recallToolName {
				continue
			}
			var args recallArgs
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				continue // malformed legacy arguments: skip rather than fail the turn
			}
			for _, k := range args.Entries {
				if k == "" {
					continue
				}
				if set == nil {
					set = make(map[string]struct{})
				}
				set[k] = struct{}{}
			}
		}
	}
	return set
}
