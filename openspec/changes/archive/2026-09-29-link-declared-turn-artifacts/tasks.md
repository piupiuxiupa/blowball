## 1. Artifact Link Extraction

- [x] 1.1 Implement `blowball://workspace/...` markdown-link extraction with URL decoding, path validation, duplicate removal, deterministic ordering, and invalid-path WARN behavior.
- [x] 1.2 Add focused parser tests for ordinary, encoded, duplicate, hidden, `tmp/`, traversal, absolute, query/fragment, and malformed links.
- [x] 1.3 Change `artifact.Service.FinalizeTurn` to accept assistant content and snapshot only extracted paths; remove workspace mtime scanning and its obsolete tests.
- [x] 1.4 Add service tests proving linked regular non-empty files are announced and missing, empty, symlink, and directory targets are skipped without failing the turn.

## 2. Turn Wiring

- [x] 2.1 Change `TurnHooks.FinalizeTurn` to receive the current turn's collected top-level assistant text, excluding sub-agent run events.
- [x] 2.2 Invoke the hook with that text at the existing `EventDone` boundary and verify artifact events remain immediately before `done` and its `meta.artifacts` summary.
- [x] 2.3 Update `MessageStreamHandler` wiring so artifact finalization no longer uses `userMsgTime`; add a concurrent-session test showing each run sees only its own linked deliverables.

## 3. Prompt and Record Persistence

- [x] 3.1 Strengthen `renderWorkspaceConvention` to require one artifact link for every final deliverable and state that unlinked deliverables are omitted from artifact records.
- [x] 3.2 Update prompt-rendering tests for the stronger requirement.
- [x] 3.3 Add a message-persistence regression test proving emitted artifact events survive as `event_type="artifact"` rows and remain excluded from reconstructed LLM context.

## 4. Verification

- [x] 4.1 Run focused package tests for artifact extraction/finalization, prompt rendering, adapter event ordering, and message persistence.
- [x] 4.2 Run `go test ./internal/...` and `go vet ./...`.
- [x] 4.3 Run OpenSpec validation for this change and resolve any spec-format failures.
