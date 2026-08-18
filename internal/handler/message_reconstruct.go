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
func MessagesToAgentMessages(prior []model.Message) ([]agent.Message, error) {
	out, _, err := MessagesToAgentMessagesIndexed(prior)
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
func MessagesToAgentMessagesIndexed(prior []model.Message) ([]agent.Message, []int, error) {
	if len(prior) == 0 {
		return nil, nil, nil
	}

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
		// belongs in the main prompt history.
		if msg.Agent == model.AgentChongzhi || msg.Agent == model.AgentLiang {
			continue
		}

		switch msg.EventType {
		case model.EventTypeMessage:
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
			if state.toolCallsPending() {
				state.flushToolCalls(&out, &lastRow)
			}
			if msg.Agent != state.currentAgent {
				state.flushTokensAndReasoning(&out, &lastRow)
				state.currentAgent = msg.Agent
			}
			state.tokenContent += msg.Content
			state.tokenLastRow = i

		case model.EventTypeReasoning:
			if msg.Role != model.RoleAssistant {
				continue
			}
			if state.toolCallsPending() {
				state.flushToolCalls(&out, &lastRow)
			}
			if msg.Agent != state.currentAgent {
				state.flushTokensAndReasoning(&out, &lastRow)
				state.currentAgent = msg.Agent
			}
			state.reasoningContent += msg.Content
			state.tokenLastRow = i

		case model.EventTypeToolCall:
			if msg.Role != model.RoleAssistant {
				continue
			}
			if state.tokensPending() {
				state.flushTokensAndReasoning(&out, &lastRow)
			}
			if msg.Agent != state.toolCallAgent {
				state.flushToolCalls(&out, &lastRow)
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
	tokenContent     string
	reasoningContent string
	currentAgent     string
	toolCalls        []toolCallEntry
	toolCallAgent    string
	toolResults      []toolResultEntry
	// tokenLastRow tracks the source row of the tokens/reasoning being
	// accumulated, so flushTokensAndReasoning can attribute the merged
	// assistant message to its last contributing row.
	tokenLastRow int
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
	return s.tokenContent != "" || s.reasoningContent != ""
}
func (s *reconstructState) toolCallsPending() bool { return len(s.toolCalls) > 0 }

func (s *reconstructState) flush(out *[]agent.Message, lastRow *[]int) {
	s.flushToolCalls(out, lastRow)
	s.flushTokensAndReasoning(out, lastRow)
}

func (s *reconstructState) flushTokensAndReasoning(out *[]agent.Message, lastRow *[]int) {
	if s.tokenContent == "" && s.reasoningContent == "" {
		return
	}
	*out = append(*out, agent.Message{
		Role:             "assistant",
		Content:          s.tokenContent,
		ReasoningContent: s.reasoningContent,
	})
	*lastRow = append(*lastRow, s.tokenLastRow)
	s.tokenContent = ""
	s.reasoningContent = ""
	s.currentAgent = ""
}

func (s *reconstructState) flushToolCalls(out *[]agent.Message, lastRow *[]int) {
	if len(s.toolCalls) == 0 {
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
		*out = append(*out, agent.Message{
			Role:      "assistant",
			ToolCalls: paired,
		})
		*lastRow = append(*lastRow, unitLast)
		*out = append(*out, toolMsgs...)
		*lastRow = append(*lastRow, toolMsgRows...)
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
