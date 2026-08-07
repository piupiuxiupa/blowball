## 1. Handler change

- [x] 1.1 In `internal/handler/message_stream.go` `SendMessage`, change the `res.err != nil` branch so that non-cancellation orchestrator errors route through the existing `persistEvents(res.events, res.usage)` closure (which writes user message + merged events + `turn_usage` + title gen), instead of logging and returning with nothing persisted.
- [x] 1.2 Keep the `errors.Is(res.err, context.Canceled)` client-disconnect branch and its distinct `Warn` log unchanged; update the non-cancellation `Error` log to include `event_count` and indicate the partial turn is being persisted. Update the surrounding code comment that claims non-cancellation errors are "discarded unchanged".

## 2. Update tests that assert the old drop-on-error behavior

- [x] 2.1 Rewrite `TestSendMessage_OrchestratorFailure_PersistsNothing` in `internal/handler/session_test.go` to assert the new behavior: the user message + the stub's emitted events (e.g. `agent_start` + token) ARE persisted in a single batch on a non-cancellation error. Rename it to reflect the new contract.
- [x] 2.2 Rewrite `TestMessageFlow_OrchestratorFailure_PersistsNothing` in `test/integration/message_flow_test.go` to assert the user message + the streamed token (`"Hello"`) ARE persisted to the MySQL tier when the scripted LLM returns an error after emitting tokens. Rename it accordingly.

## 3. Add new tests for the provider-error scenarios

- [x] 3.1 Add a handler test mirroring `TestSendMessage_ContextCanceled_PersistsUserAndPartialEvents`: a non-cancellation error returned after several events are emitted results in 1 batch with the user message + merged partial assistant events (mirror the cancel test's assertions on batch size, event types, merged token content).
- [x] 3.2 Add a handler test mirroring `TestSendMessage_ContextCanceled_NoAssistantEvents_PersistsOnlyUser`: a non-cancellation error before any assistant event persists only the user message.
- [x] 3.3 Add a handler test mirroring `TestSendMessage_ContextCanceled_FirstTurnGeneratesTitle`: a non-cancellation error on a first turn triggers title generation with the partial assistant content.
- [x] 3.4 Add a handler test asserting `turn_usage` is recorded for a failed turn whose `done` event carries usage (mirror any existing `turn_usage` assertion pattern in the handler test env; tolerate the error-path `usage.error` field).

## 4. Verification

- [x] 4.1 Run `go test ./internal/handler/...` and confirm all handler tests pass.
- [x] 4.2 Run `go test ./test/integration/...` and confirm all integration tests pass.
- [x] 4.3 Run `make lint` and resolve any new findings.
