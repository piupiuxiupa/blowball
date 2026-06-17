## 1. Handler core changes

- [x] 1.1 Remove synchronous `SaveMessage` call for user message in `internal/handler/session.go:SendMessage`
- [x] 1.2 Capture user message timestamp before orchestrator starts; keep `isFirstTurn` detection based on `prior`
- [x] 1.3 Move user message construction into the async persistence goroutine and prepend it to the assistant message slice
- [x] 1.4 Add `UserMessage` helper in `internal/handler/event_mapper.go` to build a `model.Message` for user input without using `StreamEvent`
- [x] 1.5 Update comments in `SendMessage` to reflect new flow (no sync user message save)

## 2. Test updates

- [x] 2.1 Update `internal/handler/session_test.go` happy path to expect single `SaveMessagesBatch` call containing user + assistant messages
- [x] 2.2 Update `internal/handler/session_test.go` orchestrator failure path to expect zero persistence calls
- [x] 2.3 Update `internal/handler/session_test.go` marker/tool_call path to expect single batch of length 8 (1 user + 7 assistant)
- [x] 2.4 Update `test/integration/message_flow_test.go` happy path to assert single FS/MySQL/Redis write per tier
- [x] 2.5 Update `test/integration/message_flow_test.go` orchestrator failure path to expect zero messages persisted
- [x] 2.6 Add/extend unit tests for `UserMessage` helper and mixed batch ordering

## 3. Spec and documentation

- [x] 3.1 Review delta spec `openspec/changes/async-user-message-batch/specs/session-management/spec.md` for completeness
- [x] 3.2 Update any inline code comments or docs referencing "sync user message save"

## 4. Verification

- [x] 4.1 Run `make test` (or equivalent) and ensure handler + integration tests pass
- [x] 4.2 Manually verify first-token latency improvement if possible
- [x] 4.3 Archive change after implementation and spec sync
