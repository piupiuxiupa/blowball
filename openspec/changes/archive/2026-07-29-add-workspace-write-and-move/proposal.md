## Why

The workspace REST API can read a file's text content (`GET .../content`) but has no symmetric write endpoint: the only way for the frontend to change a file's text is to re-upload the whole file as multipart, which (a) is non-atomic — `c.SaveUploadedFile` writes directly to the destination, so a crash mid-upload leaves a truncated/corrupt file, and (b) couples the destination filename to the uploaded part's filename (no rename-on-write). Meanwhile the existing move/rename endpoint (`PUT .../*path`) already supports cross-directory moves, but cannot express the most natural "drag a file into a folder" gesture: when `new_path` resolves to an existing directory it returns 409 instead of moving the source inside it, and it can never overwrite a destination. This change closes both gaps with minimal, filesystem-only additions — no database, Redis, or new dependencies.

## What Changes

- **Add `PUT /api/v1/workspace/files/*path/content`** — atomic text-content write, symmetric to the existing `GET .../content`. Create-or-replace (HTTP PUT semantics, matching the agent's `xizhi_write_file`); atomic via a temp file in the same directory followed by `os.Rename` (mirrors the existing `onlyOfficePersist` path). Path validated with `xizhi.ValidatePathAllowReserved`; request body capped at `maxUploadBytes`. Text-only: content containing a NUL byte is rejected with `400 BINARY_FILE` to stay symmetric with the read side (which refuses to return binary); binary/large files continue to use upload.
- **PUT catch-all gains suffix dispatch** — bare `PUT .../files/*path` continues to rename/move as today; `PUT .../files/*path/content` routes to the new write handler. Mirrors the existing GET `dispatchWorkspaceFile` suffix pattern (`/content`, `/download`); no new top-level route shape, and gin's wildcard/static-sibling restriction is respected.
- **Rename/move UX enhancements** on `PUT .../files/*path`:
  - **Move-into-folder**: when `new_path` resolves to an *existing directory*, the source is moved inside it as `new_path/<basename(src)>` instead of returning 409. *(Behavior change: this specific case previously returned 409.)*
  - **Optional `overwrite` flag** (`{"new_path": "...", "overwrite": true}`): when the final destination already exists, replace it instead of returning 409. Overwrite replaces an existing *file* atomically via `os.Rename`; overwriting a non-empty directory is rejected (POSIX `ENOTEMPTY`) — directory merge is out of scope. Default `false` preserves today's 409 behavior.
- **Batch move** (moving several selections at once) is explicitly out of scope for this change and noted as future work.

## Capabilities

### New Capabilities
<!-- None: both deliverables extend existing capabilities. -->

### Modified Capabilities
- `workspace-api`: add a new requirement for writing file text content (`PUT .../content`), symmetric to the existing "Get file content as text" read requirement. Atomic, create-or-replace, text-only, path-scoped.
- `workspace-file-rename`: relax the current "never overwrite an existing destination (409)" requirement — add move-into-folder semantics (destination resolves to an existing directory → move the source inside it) and an optional `overwrite` flag that replaces an existing file destination. Default behavior is unchanged when the flag is absent and the destination is a file.

## Impact

- **Code**:
  - `internal/handler/workspace.go`: new `WriteContent` handler + a shared atomic-write helper (factor the temp-then-`os.Rename` pattern already in `onlyOfficePersist`); modify `Rename` for move-into-folder + `overwrite`.
  - `internal/handler/router.go`: add a PUT suffix dispatcher parallel to `dispatchWorkspaceFile`; the existing `authed.PUT(.../files/*path)` routes through it.
  - `cmd/blowball/serve.go`: wire the new `WorkspaceWriteContent` handler into `RouteDeps`.
  - `api/openapi.yaml`: add the `/content` PUT operation + request/response schemas; add optional `overwrite` to `RenameRequest`.
  - `internal/handler/workspace_test.go`: new tests for WriteContent (create, atomic overwrite, NUL-byte rejection, too-large, path-escape, directory target) and for the rename enhancements (move-into-folder, overwrite file, overwrite non-empty directory rejected, `overwrite` default false → 409 preserved).
  - `CLAUDE.md`: extend the workspace route table with the new `PUT .../content` row and the rename body change.
- **API surface**: one new endpoint (`PUT .../content`); one additive body field (`overwrite`) on rename. No removals, no method changes.
- **Persistence / dependencies**: none — pure filesystem operations under the existing per-user workspace root; the three-layer (Redis/FS/MySQL) message pipeline and Landlock/bwrap policies are unaffected.
- **Backward compatibility**: rename without `overwrite` keeps today's 409-on-existing-file behavior; the only observable change is that an existing-*directory* destination now succeeds (move-into-folder) instead of returning 409.
- **Service roles**: content write and rename are CRUD → wired by `wireAPI` and available in the `api` (and `all`) role; no new dependency on the agent layer is introduced.
