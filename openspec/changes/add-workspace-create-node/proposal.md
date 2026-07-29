## Why

The workspace REST API can create a *file* indirectly — text via the atomic create-or-replace `PUT .../content` (added in `add-workspace-write-and-move`), binary/large via `POST .../upload` — but it has **no way to create an empty directory**, the single most basic file-manager gesture ("new folder"). Today a directory only ever appears as a *parent* auto-created by a file write (`MkdirAll(filepath.Dir(abs))`); there is no operation whose target is the directory itself, so the frontend cannot create an empty folder. There is also no explicit "create new empty file" primitive distinct from "save content". This change adds one unified, strict create endpoint that handles both files and directories via a `type` parameter, closing the empty-folder gap and giving the frontend a clean "new node" operation.

## What Changes

- **Add `POST /api/v1/workspace/files/*path`** — a unified create endpoint. The request body `{"type": "file" | "directory"}` selects what to create; the path comes from the URL catch-all (consistent with rename, whose params live in the body). Strict create semantics: if the target leaf already exists (file *or* directory) it returns **409 `ALREADY_EXISTS`** — file and directory behave identically here. Missing parent directories are **auto-created** (`MkdirAll(filepath.Dir(abs))`), so a leaf-strict create can still establish a nested path in one call.
- **Atomic, race-free strict primitives** — file creation uses `os.OpenFile(abs, O_CREATE|O_EXCL|O_WRONLY, 0o644)` and directory creation uses `os.Mkdir(abs, 0o755)`; both surface `EEXIST` on an existing leaf, so there is no check-then-create window (two concurrent creates of the same path: one wins, the other gets 409).
- **Routing** — the `/*path` catch-all under `POST` is currently unused (`POST /workspace/upload` is a separate static segment), so the new endpoint registers directly as `authed.POST("/workspace/files/*path", deps.WorkspaceCreate)` with **no suffix dispatcher** — unlike GET/PUT, POST has no `/content` split.
- **Path & input validation** — paths are validated with `xizhi.ValidatePathAllowReserved` (same as Content/Upload/Rename/Delete on the REST side, so users can manage their own `.blowball/`); creating the workspace root itself (empty/`/` path) returns **400 `BAD_REQUEST`**; a missing or invalid `type` returns **400**.
- **Response** — `200 {"path": <rel>, "type": <"file"|"directory">}`; auth via the existing `authed` group (401 missing).
- **Not a separate "create empty file" shortcut for content** — creating an *empty* file is supported, but writing *content* continues to use `PUT .../content` (create-or-replace). The two are intentionally distinct: create is "must not exist" (strict), write-content is "save regardless" (replace). This is additive only; no existing endpoint or contract changes.

## Capabilities

### New Capabilities
<!-- None: this extends an existing capability. -->

### Modified Capabilities
- `workspace-api`: add a new requirement **"Create empty file or directory"** — a strict create (409 `ALREADY_EXISTS` if the leaf already exists, identical for files and directories), auto-creating missing parent directories, selecting file vs. directory via the request body `type`, path-scoped via `xizhi.ValidatePathAllowReserved`. This fills the gap left by the existing read/content/upload/delete requirements, none of which can create an empty directory.

## Impact

- **Code**:
  - `internal/handler/workspace.go`: new `WorkspaceHandler.Create` handler + `createNodeRequest` (`Type string`) and a `createNodeResponse` (`{path, type}`).
  - `internal/handler/router.go`: add `WorkspaceCreate gin.HandlerFunc` to `RouteDeps` (with a doc comment matching the others) and register `authed.POST("/workspace/files/*path", deps.WorkspaceCreate)` in `RegisterAPIRoutes`; document it in the `RegisterRoutes` route comment block.
  - `cmd/blowball/serve.go`: wire `WorkspaceCreate: h.Create` in `wireAPI` (api role) next to the other workspace handlers; the `all` role and integration harness pick it up via shared wiring.
  - `test/integration/harness_test.go` (and any other `RouteDeps` builder): set the new field so the suite compiles.
- **API surface**: one new endpoint (`POST .../files/*path`); additive only, no removals, no method or body changes to existing endpoints.
- **OpenAPI**: add the `POST /api/v1/workspace/files/{path}` operation to `api/openapi.yaml` with `CreateNodeRequest` (`type`, required, enum `file`|`directory`) and the `{path, type}` response; document 400 (root / bad type), 403, 409 `ALREADY_EXISTS`, 401. Copy into `blowball-frontend/` and regenerate types per repo convention.
- **Tests** (`internal/handler/workspace_test.go`): create file (200, empty file on disk, parents auto-created), create directory (200), nested create auto-builds parents, 409 on existing file, 409 on existing directory, root create → 400, missing/invalid `type` → 400, path-escape → 403, missing auth → 401; plus a production-routing test (like `TestTokenDownload_ProductionRouting`) asserting the POST catch-all routes to `Create`.
- **Docs**: extend the `CLAUDE.md` workspace route table with the new `POST .../files/*path` row.
- **Persistence / dependencies**: none — pure filesystem operations under the existing per-user workspace root; the three-layer (Redis/FS/MySQL) message pipeline and Landlock/bwrap policies are unaffected.
- **Backward compatibility**: fully additive; no existing endpoint, contract, or behavior changes.
- **Service roles**: create is CRUD → wired by `wireAPI` and available in the `api` (and `all`) role; no new dependency on the agent layer.
