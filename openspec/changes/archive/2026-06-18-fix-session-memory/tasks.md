## 1. Stream events and tool result persistence

- [x] 1.1 Add `EventToolResult` constant and `ToolResultEvent(agent, toolCallID, output string) StreamEvent` constructor in `internal/stream/event.go`.
- [x] 1.2 Update `ToolCallEvent` signature/implementation to carry `tool_call_id` (e.g. add parameter or store in `Meta`).
- [x] 1.3 Emit `ToolResultEvent` in `internal/agent/confuse.go` after `dispatchToolCalls` completes, mapping each `tool_call_id` to its result content.
- [x] 1.4 Emit `ToolResultEvent` in `internal/agent/chongzhi.go` and `internal/agent/liang.go` after their respective tool dispatch loops.

## 2. Message persistence mapping

- [x] 2.1 Update `MessageFromEvent` in `internal/handler/event_mapper.go` to handle `EventToolResult`: set `Role = model.RoleTool`, `EventType = model.EventTypeToolResult`, and serialize content as `{"tool_call_id":"...","output":...}`.
- [x] 2.2 Update tool_call persistence shape in `MessageFromEvent` to include `tool_call_id` in the JSON content (`{"tool_call_id":"...","name":"...","args":...}`).
- [x] 2.3 Add constants `EventTypeToolResult` and `RoleTool` in `internal/model/message.go` if not already present.

## 3. Conversation history reconstruction

- [x] 3.1 Implement `MessagesToAgentMessages(prior []model.Message) ([]agent.Message, error)` in `internal/handler` (or a new helper file) that:
  - maps `RoleUser` / `EventTypeMessage` rows to `agent.Message{Role: "user"}`;
  - merges consecutive `RoleAssistant` / `EventTypeToken` rows with the same `Agent` into a single assistant message;
  - maps paired `tool_call` + `tool_result` rows into an assistant message with `ToolCalls` followed by `role = tool` messages;
  - ignores marker events (`agent_start`, `agent_end`, `agent_error`) and sub-agent events (`AgentChongzhi`, `AgentLiang`).
- [x] 3.2 Add unit tests for reconstruction covering: plain text conversation, single tool call/result, parallel tool calls, missing tool result, sub-agent events ignored.

## 4. Orchestrator interface and wiring

- [x] 4.1 Change `OrchestratorRunner.Handle` signature in `internal/handler/ports.go` to accept `messages []agent.Message` instead of `userMessage string`.
- [x] 4.2 Update `orchestratorAdapter.Handle` to forward the `messages` slice to `inner.Handle`.
- [x] 4.3 Update `agent.Orchestrator.Handle` in `internal/agent/orchestrator.go` to accept `messages []agent.Message` and pass it directly to `confuse.Run` (remove the single-message wrap).
- [x] 4.4 Update `stubOrchestrator` in `internal/handler/session_test.go` and any other test stubs to match the new signature.

## 5. Handler integration

- [x] 5.1 In `SessionHandler.SendMessage`, call `MessagesToAgentMessages(prior)` and append the current user message to produce the full turn messages.
- [x] 5.2 Pass the full message slice to `h.orch.Handle` instead of `req.Content`.
- [x] 5.3 Keep `isFirstTurn := len(prior) == 0` logic unchanged for title generation.

## 6. Tests

- [x] 6.1 Update existing handler and agent unit tests to compile and pass with the new `OrchestratorRunner` signature.
- [x] 6.2 Add an integration test in `test/integration/message_flow_test.go` that sends two turns in the same session and asserts the second turn's LLM prompt contains the first turn's user message and assistant reply.
- [x] 6.3 Add an integration test for tool-call memory: first turn triggers a tool, second turn references the tool result, and the reconstructed history contains both the tool call and tool result.

## 7. Verification

- [x] 7.1 Run `go test ./internal/...` and `go test ./test/integration/...` (or equivalent) and fix failures.
- [x] 7.2 Run lint/format checks (`gofmt`, `go vet`) on changed packages.
- [x] 7.3 Perform a manual smoke test via the API: send two messages in one session and confirm the second response references the first. *(Not run manually; equivalent coverage provided by `TestMessageFlow_TwoTurns_PromptContainsHistory` and `TestMessageFlow_ToolCallMemory` integration tests.)*
