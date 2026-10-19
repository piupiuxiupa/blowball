package handler

import (
	"encoding/json"
	"fmt"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
)

// MessagesToAgentMessages converts the ordered persistence event stream returned
// by MessageService.RecoverMessages into an []agent.Message slice suitable for
// the OpenAI chat completion format.
//
// Reconstruction rules:
//   - user message rows (event_type=message, role=user) map directly to
//     role="user" messages.
//   - consecutive token and/or reasoning rows from the same agent are merged
//     into a single role="assistant" message carrying both Content and
//     ReasoningContent.
//   - consecutive tool_call rows from the same agent are grouped into a single
//     role="assistant" message with ToolCalls; following tool_result rows with
//     matching tool_call_id become role="tool" messages. Unpaired tool_calls
//     are omitted.
//   - marker rows (agent_start, agent_end, agent_error) and rows produced by
//     sub-agents (Chongzhi, Liang) are ignored.
func MessagesToAgentMessages(prior []model.Message, calls []model.ToolCall) ([]agent.Message, error) {
	out, _, err := MessagesToAgentMessagesIndexed(prior, calls)
	return out, err
}

// MessagesToAgentMessagesIndexed is MessagesToAgentMessages plus row
// attribution: lastRow[i] is the index into prior of the last persistence row
// that contributed to out[i] (the user row for user messages; the last merged
// token/reasoning row for assistant text; the paired call/result rows for
// tool units). The context-compaction capability uses it to map an
// agent-message range boundary back to the persistence composite cursor
// (msg_time, msg_index, id) and to locate a prior boundary inside the
// reconstructed sequence.
//
// Rows are grouped by the composite key (agent, run_id): interleaved rows of
// concurrent sub-agent invocations (subagent-run-identity capability) replay
// into per-run fragments instead of merging across runs. A NULL/empty run_id
// degenerates to the agent name alone — the pre-change behavior for top-level
// turns.
// The calls parameter carries the session's tool_calls rows
// (refactor-message-storage): tool PLACEHOLDER rows (event_type=tool_call
// with a non-empty ToolCallID) join them by tool_call_id to rebuild the
// wire-format tool unit. Legacy event rows without ToolCallID reconstruct
// from their JSON event content as before.
func MessagesToAgentMessagesIndexed(prior []model.Message, calls []model.ToolCall) ([]agent.Message, []int, error) {
	if len(prior) == 0 {
		return nil, nil, nil
	}
	callsByID := indexToolCallsByID(calls)

	var out []agent.Message
	var lastRow []int
	state := &reconstructState{}

	for i := range prior {
		msg := prior[i]

		// Skip marker events and any row with an empty role.
		if msg.Role == "" {
			continue
		}

		// Ignore sub-agent events; only the top-level agent (Confucius) conversation
		// belongs in the main prompt history. A spawn PLACEHOLDER row keeps
		// its instance id purely as the transcript link — it is a top-level
		// call (ToolCallID set) and stays in the history.
		if msg.Agent == model.AgentChongzhi || msg.Agent == model.AgentLiang ||
			(msg.AgentInstanceID != "" && msg.ToolCallID == "") {
			continue
		}

		switch msg.EventType {
		case model.EventTypeMessage:
			// Assistant FINAL row (refactor-message-storage): event_type
			// message + role assistant is the final-row discriminator
			// (legacy assistant rows carry token/reasoning/tool_call event
			// types); content is already merged, and the following tool
			// placeholder rows carry the wire-format tool unit.
			if msg.Role == model.RoleAssistant && msg.EventType == model.EventTypeMessage {
				state.flush(&out, &lastRow)
				state.beginFinal(msg.Content, msg.ReasoningContent, i)
				continue
			}
			if msg.Role != model.RoleUser {
				continue
			}
			state.flush(&out, &lastRow)
			out = append(out, agent.Message{Role: "user", Content: msg.Content})
			lastRow = append(lastRow, i)

		case model.EventTypeToken:
			if msg.Role != model.RoleAssistant {
				continue
			}
			if state.afterResult {
				state.flush(&out, &lastRow)
			}
			state.noteTextTrace(msg.TraceID)
			state.appendToken(runGroupKeyOf(msg), msg.Content, i)

		case model.EventTypeReasoning:
			if msg.Role != model.RoleAssistant {
				continue
			}
			if state.afterResult {
				state.flush(&out, &lastRow)
			}
			state.noteTextTrace(msg.TraceID)
			state.appendReasoning(runGroupKeyOf(msg), msg.Content, i)

		case model.EventTypeToolCall:
			if msg.Role != model.RoleAssistant {
				continue
			}
			// Placeholder row (refactor-message-storage): the call and its
			// result both come from the canonical tool_calls row; the
			// placeholder's own position is the ordering anchor.
			if msg.ToolCallID != "" {
				if state.tokensPending() && state.textTraceSet && msg.TraceID != state.textTraceID {
					// Text from another turn (for example, the exchange before a
					// send-time recall pair) is already complete; only same-turn
					// narration joins the following tool unit.
					state.flush(&out, &lastRow)
				}
				c, ok := callsByID[msg.TraceID+""+msg.ToolCallID]
				if !ok || (c.Result == "" && c.Status != model.ToolCallStatusCompleted) {
					continue // unpaired/pending: skipped like the legacy path
				}
				state.toolCalls = append(state.toolCalls, toolCallEntry{
					call: agent.ToolCall{
						ID: c.ToolCallID,
						Function: agent.ToolCallFunction{
							Name:      c.Name,
							Arguments: c.Arguments,
						},
					},
					row: i,
				})
				state.toolResults = append(state.toolResults, toolResultEntry{
					toolCallID: c.ToolCallID,
					output:     c.Result,
					row:        i,
				})
				state.afterResult = true
				continue
			}
			if state.afterResult {
				state.flush(&out, &lastRow)
			}
			if state.tokensPending() && state.textTraceSet && msg.TraceID != state.textTraceID {
				state.flush(&out, &lastRow)
			}
			// Tool-call batching keys on the agent name alone, NOT the
			// (agent, run_id) composite: results may interleave arbitrarily
			// after their calls (parallel dispatch), and flushing a batch on
			// a run-key change would strand calls whose results have not
			// arrived yet. Pairing is by tool_call_id inside the batch —
			// unaffected by run grouping by construction.
			if msg.Agent != state.toolCallAgent {
				if state.toolCallsPending() {
					state.flush(&out, &lastRow)
				}
				state.toolCallAgent = msg.Agent
			}
			call, err := parseToolCall(msg.Content)
			if err != nil {
				return nil, nil, fmt.Errorf("parse tool_call at index %d: %w", i, err)
			}
			// Old rows may lack a tool_call_id; they cannot be paired with results,
			// so skip them rather than presenting an incomplete tool-calling turn.
			if call.ID == "" {
				continue
			}
			state.toolCalls = append(state.toolCalls, toolCallEntry{call: call, row: i})

		case model.EventTypeToolResult:
			if msg.Role != model.RoleTool {
				continue
			}
			if !state.toolCallsPending() {
				continue
			}
			toolCallID, output, err := parseToolResult(msg.Content)
			if err != nil {
				return nil, nil, fmt.Errorf("parse tool_result at index %d: %w", i, err)
			}
			state.toolResults = append(state.toolResults, toolResultEntry{
				toolCallID: toolCallID,
				output:     output,
				row:        i,
			})
			state.afterResult = true

		default:
			// Other event types are not part of the prompt history.
		}
	}

	state.flush(&out, &lastRow)
	return out, lastRow, nil
}

// reconstructState accumulates partially-built assistant messages while scanning
// the persisted event stream.
type reconstructState struct {
	// tokenRuns accumulates token/reasoning content per (agent, instance, run)
	// in first-seen order: interleaved rows of concurrent sub-agent
	// invocations (subagent-run-identity capability) regroup into one
	// coherent fragment per run instead of merging across runs. Rows without
	// run identity all share the agent-only key, which degenerates to the
	// pre-change single-accumulator behavior.
	tokenRuns     []runFragment
	tokenRunIndex map[runGroupKey]int
	// final is the provider-native assistant prefix of a final-row round. It
	// stays buffered until the round's adjacent placeholders are known so a
	// narrated response reconstructs as one assistant message.
	final         *assistantFragment
	textTraceID   string
	textTraceSet  bool
	afterResult   bool
	toolCalls     []toolCallEntry
	toolCallAgent string
	toolResults   []toolResultEntry
}

// assistantFragment is the text half of one assistant round.
type assistantFragment struct {
	content   string
	reasoning string
	row       int
}

// runFragment is one run's accumulated token/reasoning text. lastRow is the
// source row of the fragment's last contributing row, for boundary
// attribution.
type runFragment struct {
	key       runGroupKey
	content   string
	reasoning string
	lastRow   int
}

// runGroupKey is the identity composite that one reconstructed
// fragment belongs to. Empty identities are the pre-change shape: the key
// degenerates to the agent name, so top-level turns group exactly as before.
type runGroupKey struct {
	agent    string
	instance string
	runID    string
}

// runGroupKeyOf returns msg's identity grouping key.
func runGroupKeyOf(msg model.Message) runGroupKey {
	return runGroupKey{agent: msg.Agent, instance: msg.AgentInstanceID, runID: msg.RunID}
}

// indexToolCallsByID buckets the session's tool_calls rows by
// (turn, tool_call_id), so a placeholder row joins its canonical payload
// with a single map hit.
func indexToolCallsByID(calls []model.ToolCall) map[string]model.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make(map[string]model.ToolCall, len(calls))
	for _, c := range calls {
		out[c.TurnID+""+c.ToolCallID] = c
	}
	return out
}

// appendToken adds a token fragment to its run's accumulator (no-op on empty
// content, matching the prior single-accumulator behavior of not counting
// empty rows as pending).
func (s *reconstructState) appendToken(key runGroupKey, content string, row int) {
	if content == "" {
		return
	}
	f := s.runFragment(key, row)
	f.content += content
	f.lastRow = row
}

// appendReasoning adds a reasoning fragment to its run's accumulator.
func (s *reconstructState) appendReasoning(key runGroupKey, content string, row int) {
	if content == "" {
		return
	}
	f := s.runFragment(key, row)
	f.reasoning += content
	f.lastRow = row
}

// runFragment returns the mutable accumulator for key, creating it (in
// first-seen order) when this is the key's first fragment.
func (s *reconstructState) runFragment(key runGroupKey, row int) *runFragment {
	if idx, ok := s.tokenRunIndex[key]; ok {
		return &s.tokenRuns[idx]
	}
	if s.tokenRunIndex == nil {
		s.tokenRunIndex = make(map[runGroupKey]int)
	}
	s.tokenRunIndex[key] = len(s.tokenRuns)
	s.tokenRuns = append(s.tokenRuns, runFragment{key: key, lastRow: row})
	return &s.tokenRuns[len(s.tokenRuns)-1]
}

type toolCallEntry struct {
	call agent.ToolCall
	row  int
}

type toolResultEntry struct {
	toolCallID string
	output     string
	row        int
}

func (s *reconstructState) tokensPending() bool {
	return len(s.tokenRuns) > 0
}
func (s *reconstructState) toolCallsPending() bool { return len(s.toolCalls) > 0 }

func (s *reconstructState) beginFinal(content, reasoning string, row int) {
	s.final = &assistantFragment{content: content, reasoning: reasoning, row: row}
}

func (s *reconstructState) noteTextTrace(traceID string) {
	if len(s.tokenRuns) > 0 {
		return
	}
	s.textTraceID = traceID
	s.textTraceSet = true
}

func (s *reconstructState) flush(out *[]agent.Message, lastRow *[]int) {
	prefix, hasPrefix, multipleRuns := s.assistantPrefix()
	if hasPrefix {
		s.flushToolCallsWithPrefix(out, lastRow, prefix)
		s.final = nil
		if !multipleRuns {
			s.tokenRuns = nil
			s.tokenRunIndex = nil
			s.afterResult = false
			return
		}
	} else {
		s.flushToolCallsWithPrefix(out, lastRow, nil)
	}
	s.flushTokensAndReasoning(out, lastRow)
	s.afterResult = false
}

// assistantPrefix selects the text to combine with a pending tool unit. A
// final row is authoritative. Legacy token rows combine when there is exactly
// one run (the normal top-level case); multiple identity-carrying runs remain
// separate to avoid garbling concurrent sub-agent histories.
func (s *reconstructState) assistantPrefix() (*assistantFragment, bool, bool) {
	if s.final != nil {
		return s.final, true, len(s.tokenRuns) > 0
	}
	if len(s.tokenRuns) == 1 {
		f := s.tokenRuns[0]
		return &assistantFragment{content: f.content, reasoning: f.reasoning, row: f.lastRow}, true, false
	}
	return nil, false, len(s.tokenRuns) > 1
}

// flushTokensAndReasoning emits every accumulated run fragment as its own
// assistant message, in first-seen run order. For rows without run identity
// there is exactly one fragment, so the output is byte-for-byte the
// pre-change single-message flush.
func (s *reconstructState) flushTokensAndReasoning(out *[]agent.Message, lastRow *[]int) {
	for _, f := range s.tokenRuns {
		*out = append(*out, agent.Message{
			Role:             "assistant",
			Content:          f.content,
			ReasoningContent: f.reasoning,
		})
		*lastRow = append(*lastRow, f.lastRow)
	}
	s.tokenRuns = nil
	s.tokenRunIndex = nil
}

func (s *reconstructState) flushToolCallsWithPrefix(out *[]agent.Message, lastRow *[]int, prefix *assistantFragment) {
	if len(s.toolCalls) == 0 {
		if prefix != nil && (prefix.content != "" || prefix.reasoning != "") {
			*out = append(*out, agent.Message{
				Role:             "assistant",
				Content:          prefix.content,
				ReasoningContent: prefix.reasoning,
			})
			*lastRow = append(*lastRow, prefix.row)
		}
		return
	}

	// Pair results with calls by tool_call_id. Preserve the original call order
	// in the assistant message and in the following tool messages.
	resultByID := make(map[string][]toolResultEntry, len(s.toolResults))
	for _, r := range s.toolResults {
		resultByID[r.toolCallID] = append(resultByID[r.toolCallID], r)
	}

	var paired []agent.ToolCall
	var pairedRows []int
	var toolMsgs []agent.Message
	var toolMsgRows []int
	for _, entry := range s.toolCalls {
		results, ok := resultByID[entry.call.ID]
		if !ok {
			continue
		}
		paired = append(paired, entry.call)
		pairedRows = append(pairedRows, entry.row)
		for _, r := range results {
			toolMsgs = append(toolMsgs, agent.Message{
				Role:       "tool",
				Content:    r.output,
				ToolCallID: entry.call.ID,
				Name:       entry.call.Function.Name,
			})
			toolMsgRows = append(toolMsgRows, r.row)
		}
	}

	if len(paired) > 0 {
		// The tool unit's assistant message is attributed to the LAST row of
		// the whole paired unit (calls and results) so a boundary cut at this
		// message never strands a result row on the far side of its call.
		unitLast := pairedRows[len(pairedRows)-1]
		for _, row := range pairedRows {
			if row > unitLast {
				unitLast = row
			}
		}
		for _, row := range toolMsgRows {
			if row > unitLast {
				unitLast = row
			}
		}
		assistant := agent.Message{Role: "assistant", ToolCalls: paired}
		if prefix != nil {
			assistant.Content = prefix.content
			assistant.ReasoningContent = prefix.reasoning
			if prefix.row > unitLast {
				unitLast = prefix.row
			}
		}
		*out = append(*out, assistant)
		*lastRow = append(*lastRow, unitLast)
		*out = append(*out, toolMsgs...)
		*lastRow = append(*lastRow, toolMsgRows...)
	} else if prefix != nil && (prefix.content != "" || prefix.reasoning != "") {
		*out = append(*out, agent.Message{
			Role:             "assistant",
			Content:          prefix.content,
			ReasoningContent: prefix.reasoning,
		})
		*lastRow = append(*lastRow, prefix.row)
	}

	s.toolCalls = nil
	s.toolCallAgent = ""
	s.toolResults = nil
}

// toolCallPayload mirrors the JSON persisted for a tool_call event.
type toolCallPayload struct {
	ToolCallID string         `json:"tool_call_id"`
	Name       string         `json:"name"`
	Args       map[string]any `json:"args"`
}

// toolResultPayload mirrors the JSON persisted for a tool_result event.
type toolResultPayload struct {
	ToolCallID string `json:"tool_call_id"`
	Output     any    `json:"output"`
}

func parseToolCall(content string) (agent.ToolCall, error) {
	var payload toolCallPayload
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return agent.ToolCall{}, err
	}
	argsRaw, err := json.Marshal(payload.Args)
	if err != nil {
		return agent.ToolCall{}, err
	}
	return agent.ToolCall{
		ID: payload.ToolCallID,
		Function: agent.ToolCallFunction{
			Name:      payload.Name,
			Arguments: string(argsRaw),
		},
	}, nil
}

func parseToolResult(content string) (string, string, error) {
	var payload toolResultPayload
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return "", "", err
	}
	// Normalize output back to a string for agent.Message.Content. Structured
	// outputs are re-serialized to JSON; plain strings are kept as-is.
	output, err := normalizeToolOutput(payload.Output)
	if err != nil {
		return "", "", err
	}
	return payload.ToolCallID, output, nil
}

func normalizeToolOutput(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case nil:
		return "", nil
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}
