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
	if len(prior) == 0 {
		return nil, nil
	}

	var out []agent.Message
	state := &reconstructState{}

	for i := range prior {
		msg := prior[i]

		// Skip marker events and any row with an empty role.
		if msg.Role == "" {
			continue
		}

		// Ignore sub-agent events; only the top-level agent (Confuse) conversation
		// belongs in the main prompt history.
		if msg.Agent == model.AgentChongzhi || msg.Agent == model.AgentLiang {
			continue
		}

		switch msg.EventType {
		case model.EventTypeMessage:
			if msg.Role != model.RoleUser {
				continue
			}
			state.flush(&out)
			out = append(out, agent.Message{Role: "user", Content: msg.Content})

		case model.EventTypeToken:
			if msg.Role != model.RoleAssistant {
				continue
			}
			if state.toolCallsPending() {
				state.flushToolCalls(&out)
			}
			if msg.Agent != state.currentAgent {
				state.flushTokensAndReasoning(&out)
				state.currentAgent = msg.Agent
			}
			state.tokenContent += msg.Content

		case model.EventTypeReasoning:
			if msg.Role != model.RoleAssistant {
				continue
			}
			if state.toolCallsPending() {
				state.flushToolCalls(&out)
			}
			if msg.Agent != state.currentAgent {
				state.flushTokensAndReasoning(&out)
				state.currentAgent = msg.Agent
			}
			state.reasoningContent += msg.Content

		case model.EventTypeToolCall:
			if msg.Role != model.RoleAssistant {
				continue
			}
			if state.tokensPending() {
				state.flushTokensAndReasoning(&out)
			}
			if msg.Agent != state.toolCallAgent {
				state.flushToolCalls(&out)
				state.toolCallAgent = msg.Agent
			}
			call, err := parseToolCall(msg.Content)
			if err != nil {
				return nil, fmt.Errorf("parse tool_call at index %d: %w", i, err)
			}
			// Old rows may lack a tool_call_id; they cannot be paired with results,
			// so skip them rather than presenting an incomplete tool-calling turn.
			if call.ID == "" {
				continue
			}
			state.toolCalls = append(state.toolCalls, call)

		case model.EventTypeToolResult:
			if msg.Role != model.RoleTool {
				continue
			}
			if !state.toolCallsPending() {
				continue
			}
			toolCallID, output, err := parseToolResult(msg.Content)
			if err != nil {
				return nil, fmt.Errorf("parse tool_result at index %d: %w", i, err)
			}
			state.toolResults = append(state.toolResults, toolResultEntry{
				toolCallID: toolCallID,
				output:     output,
			})

		default:
			// Other event types are not part of the prompt history.
		}
	}

	state.flush(&out)
	return out, nil
}

// reconstructState accumulates partially-built assistant messages while scanning
// the persisted event stream.
type reconstructState struct {
	tokenContent     string
	reasoningContent string
	currentAgent     string
	toolCalls        []agent.ToolCall
	toolCallAgent    string
	toolResults      []toolResultEntry
}

type toolResultEntry struct {
	toolCallID string
	output     string
}

func (s *reconstructState) tokensPending() bool {
	return s.tokenContent != "" || s.reasoningContent != ""
}
func (s *reconstructState) toolCallsPending() bool { return len(s.toolCalls) > 0 }

func (s *reconstructState) flush(out *[]agent.Message) {
	s.flushToolCalls(out)
	s.flushTokensAndReasoning(out)
}

func (s *reconstructState) flushTokensAndReasoning(out *[]agent.Message) {
	if s.tokenContent == "" && s.reasoningContent == "" {
		return
	}
	*out = append(*out, agent.Message{
		Role:             "assistant",
		Content:          s.tokenContent,
		ReasoningContent: s.reasoningContent,
	})
	s.tokenContent = ""
	s.reasoningContent = ""
	s.currentAgent = ""
}

func (s *reconstructState) flushToolCalls(out *[]agent.Message) {
	if len(s.toolCalls) == 0 {
		return
	}

	// Pair results with calls by tool_call_id. Preserve the original call order
	// in the assistant message and in the following tool messages.
	resultByID := make(map[string][]string, len(s.toolResults))
	for _, r := range s.toolResults {
		resultByID[r.toolCallID] = append(resultByID[r.toolCallID], r.output)
	}

	var paired []agent.ToolCall
	var toolMsgs []agent.Message
	for _, call := range s.toolCalls {
		results, ok := resultByID[call.ID]
		if !ok {
			continue
		}
		paired = append(paired, call)
		for _, output := range results {
			toolMsgs = append(toolMsgs, agent.Message{
				Role:       "tool",
				Content:    output,
				ToolCallID: call.ID,
				Name:       call.Function.Name,
			})
		}
	}

	if len(paired) > 0 {
		*out = append(*out, agent.Message{
			Role:      "assistant",
			ToolCalls: paired,
		})
		*out = append(*out, toolMsgs...)
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
