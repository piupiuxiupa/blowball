## 1. Add event-merge helper

- [x] 1.1 Add `MergeEvents(events []stream.StreamEvent) []stream.StreamEvent` in `internal/handler/event_mapper.go` (or a new helper file in `internal/handler`).
- [x] 1.2 Implement merge predicate: only adjacent events with the same `Agent` and `Type == EventToken` are concatenated; all other events start a new output event.
- [x] 1.3 Ensure merged token events preserve the original `Agent` and `Type` and have `Content` equal to the concatenation of all merged contents.

## 2. Wire merge into persistence path

- [x] 2.1 In `internal/handler/session.go`, call `MergeEvents(res.events)` before mapping events to `model.Message`.
- [x] 2.2 Update the `MsgIndex` assignment loop to iterate over the merged slice so indices remain monotonic.
- [x] 2.3 Verify `titleSvc.GenerateTitle` still iterates over the raw `res.events` slice, not the merged slice.

## 3. Add unit tests

- [x] 3.1 Add `TestMergeEvents` in `internal/handler/event_mapper_test.go` (create if missing) covering:
  - pure token sequence merges to one row
  - lifecycle events break token merge
  - different agents are not merged
  - tool_call events remain independent
  - sub-agent hand-off preserves order
- [x] 3.2 Update `internal/handler/session_test.go` expectations if any tests assert exact message counts for assistant turns.

## 4. Verify

- [x] 4.1 Run `go test ./internal/handler/...` and fix failures.
- [x] 4.2 Run `go test ./internal/agent/...` to ensure orchestrator adapter behavior is unchanged.
- [x] 4.3 Run `go test ./...` for full suite confidence.
