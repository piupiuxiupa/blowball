## 1. API Handler

- [x] 1.1 Add `include_hidden` query parameter parsing to `WorkspaceHandler.List` in `internal/handler/workspace.go`.
- [x] 1.2 Filter out entries whose names begin with `.` when `include_hidden` is `false` (default).
- [x] 1.3 Keep the existing directory-first, then-name sorting behavior unchanged.

## 2. OpenAPI Documentation

- [x] 2.1 Add optional `include_hidden` query parameter to `GET /api/v1/workspace/files` in `api/openapi.yaml`.
- [x] 2.2 Document the parameter as `type: boolean` with `default: false`.

## 3. Tests

- [x] 3.1 Add or update tests in `internal/handler/workspace_test.go` to verify hidden entries are excluded by default.
- [x] 3.2 Add tests to verify `include_hidden=true` returns hidden entries.
- [x] 3.3 Run `go test ./internal/handler/...` and ensure all tests pass.

## 4. Verification

- [x] 4.1 Build the server with `make build`.
- [x] 4.2 Start the server and call `GET /api/v1/workspace/files` to confirm hidden entries are excluded.
- [x] 4.3 Call `GET /api/v1/workspace/files?include_hidden=true` to confirm hidden entries are included.
