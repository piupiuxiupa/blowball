## 1. Core Implementation

- [x] 1.1 Update `internal/service/session.go` `SaveMessagesBatch` to call `s.mysql.UpdateSessionTime(ctx, msgs[0].SessionID)` after `AppendMessages`, logging errors without failing the batch.

## 2. Tests

- [x] 2.1 Add or extend unit tests for `SessionService.SaveMessagesBatch` to assert that `UpdateSessionTime` is invoked with the correct `sessionID` when messages are persisted.
- [x] 2.2 Run `go test ./internal/service/...` and `go test ./test/integration/...` to verify the change and ensure no regressions.
