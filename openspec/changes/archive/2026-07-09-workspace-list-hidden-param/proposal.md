## Why

The workspace file listing REST API currently returns all entries, including hidden files and directories (names starting with `.`). This is inconsistent with the agent-facing `xizhi_list_files`, `xizhi_tree`, and `xizhi_glob_files` tools, which already support an `include_hidden` parameter defaulting to `false`. Users browsing the workspace through the frontend file tree see clutter such as `.pip/`, `.git/`, or `.env` files. Adding the same control to the REST API aligns the two interfaces and keeps the default workspace view clean.

## What Changes

- Add an optional `include_hidden` query parameter to `GET /api/v1/workspace/files`.
  - When omitted or `false`, omit entries whose names begin with `.` (consistent with existing xizhi tool behavior).
  - When `true`, return all entries, including hidden ones.
- Update `internal/handler/workspace.go` to read the parameter and apply the filter.
- Update `api/openapi.yaml` to document the new parameter.
- Add/update handler tests to cover both default-hidden and include-hidden behavior.

## Capabilities

### New Capabilities

- None.

### Modified Capabilities

- `workspace-api`: The `GET /api/v1/workspace/files` endpoint gains a new `include_hidden` query parameter and changes its default behavior to exclude hidden entries. This is a **BREAKING** behavioral change for clients that previously relied on the endpoint returning hidden files by default.

## Impact

- Backend: `internal/handler/workspace.go`, `api/openapi.yaml`, `internal/handler/workspace_test.go`.
- Frontend: No changes required in this proposal; the file tree will simply stop showing hidden entries until a future UI toggle is added.
- Agent tools: No changes; `xizhi_list_files` / `xizhi_tree` / `xizhi_glob_files` already support `include_hidden`.
