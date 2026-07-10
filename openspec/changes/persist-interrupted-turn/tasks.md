## 1. Handler Logic

- [x] 1.1 Update `SessionHandler.SendMessage` in `internal/handler/session.go` so the `context.Canceled` path persists the user message and the partial event stream (`res.events`) using the existing detached-context `SaveMessagesBatch` goroutine.
- [x] 1.2 Ensure first-turn title generation runs for interrupted turns, using the partial assistant tokens collected in `res.events`.
- [x] 1.3 Keep non-cancellation errors on the existing discard path and add a warning log when a partial interrupted turn is saved.

## 2. Tests

- [x] 2.1 Add or update unit tests in `internal/handler/session_test.go` to verify that `context.Canceled` results in `SaveMessagesBatch` being called with the user message and at least the emitted assistant events.
- [x] 2.2 Add an integration test in `test/integration/` that interrupts an SSE request mid-stream and asserts the session history includes the user message and partial assistant content after recovery.

## 3. Verification

- [x] 3.1 Run `make test` and `make lint` and fix any failures.
- [x] 3.2 Manually verify (or via test) that reloading a session after client disconnect shows the preserved user message and partial assistant response.
