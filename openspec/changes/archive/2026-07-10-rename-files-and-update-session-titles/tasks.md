## 1. Database migration

- [x] 1.1 Create `migrations/009_titles_manual.sql` to add `is_manual BOOLEAN NOT NULL DEFAULT FALSE` to `titles` table
- [x] 1.2 Verify migration runs cleanly against local MySQL (`docker compose up -d` + `make run`)

## 2. Data model and store layer

- [x] 2.1 Add `IsManual bool` field to `internal/model/title.go`
- [x] 2.2 Update `internal/store/mysql/title.go`:
  - Include `is_manual` in `getTitleSQL` SELECT
  - Update `UpsertTitle` SQL to set `is_manual = FALSE` explicitly
  - Add `UpsertTitleManual(ctx, sessionID, title, traceID string) error` that INSERT/UPDATE with `is_manual = TRUE`
- [x] 2.3 Add `UpdateSessionTime(ctx, sessionID string) error` to `internal/store/mysql/session.go` (touch `update_time`)
- [x] 2.4 Update `internal/service/deps.go` `MySQLStore` interface to include new methods if needed

## 3. Service layer

- [x] 3.1 Update `internal/service/title.go`:
  - `GenerateTitle`: call `GetTitle` first; skip LLM and upsert if `IsManual == true`
  - Add `SetManualTitle(ctx, sessionID, userID, title string) error` that sanitizes title and calls `UpsertTitleManual` + `UpdateSessionTime`
- [x] 3.2 Add unit tests in `internal/service/title_test.go` for manual title and AI-skip behavior

## 4. Session title handler

- [x] 4.1 Add `UpdateTitle(c *gin.Context)` to `internal/handler/session.go`
  - Parse `{"title": "..."}` body
  - Verify session exists and belongs to user via `sessSvc.GetSessionByID`
  - Call `titleSvc.SetManualTitle`
  - Return 200 with `{"session_id", "title", "update_time"}`
  - Return 400 for empty title, 404 for missing/unowned session
- [x] 4.2 Add handler tests in `internal/handler/session_test.go`

## 5. Workspace rename handler

- [x] 5.1 Add `Rename(c *gin.Context)` to `internal/handler/workspace.go`
  - Parse `{"new_path": "..."}` body
  - Validate both source (`c.Param("path")`) and destination with `xizhi.ValidatePath`
  - Verify source exists with `os.Stat`
  - Verify destination does NOT exist (return 409 if it does)
  - Call `os.Rename`
  - Return 200 with `{"old_path", "new_path"}`
  - Return 403 for path outside workspace, 404 for missing source, 409 for existing destination
- [x] 5.2 Add handler tests in `internal/handler/workspace_test.go`

## 6. Router wiring

- [x] 6.1 Add `SessionUpdateTitle` and `WorkspaceRename` fields to `internal/handler/router.go` `RouteDeps`
- [x] 6.2 Register `authed.PATCH("/sessions/:session_id", deps.SessionUpdateTitle)`
- [x] 6.3 Register `authed.PUT("/workspace/files/*path", deps.WorkspaceRename)` and add PUT dispatch in `dispatchWorkspaceFile`
- [x] 6.4 Wire new deps in `cmd/server/main.go`

## 7. OpenAPI and types

- [x] 7.1 Add `PATCH /api/v1/sessions/{session_id}` to `api/openapi.yaml`
- [x] 7.2 Add `PUT /api/v1/workspace/files/{path}` to `api/openapi.yaml`
- [x] 7.3 (Optional) Run `cd frontend && npm run generate-api` to refresh types; no frontend code changes

## 8. Integration tests

- [x] 8.1 Add integration test for manual title update in `test/integration/`
- [x] 8.2 Add integration test for workspace file rename in `test/integration/`
- [x] 8.3 Run `make test` and `make lint` to verify all checks pass
