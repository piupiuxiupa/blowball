# Tasks

## 1. Atomic-write primitive

- [x] 1.1 Add `atomicWriteFile(abs string, data []byte, limit int64) (written int, err error)` in `internal/handler/workspace.go`: create temp via `os.CreateTemp(filepath.Dir(abs), ".ws-write-*")`, write `data` honoring `limit` (reject > limit), `os.Rename` over `abs` on success, remove temp on any failure.
- [x] 1.2 Refactor `onlyOfficePersist` to stream into `atomicWriteFile`'s temp step (or share the temp+rename+limit recipe), removing the duplicated temp/rename/limit block. Keep its download-then-persist behavior identical.

## 2. WriteContent handler

- [x] 2.1 Add `writeContentRequest` (`Content string`) and reuse `UploadResponse`-shaped `{path, size}` for the response.
- [x] 2.2 Implement `WorkspaceHandler.WriteContent`: resolve `userID`/`tid`/`ctx`; apply `http.MaxBytesReader(c.Writer, c.Request.Body, h.maxUploadBytes)` when cap > 0; `ShouldBindJSON`.
- [x] 2.3 Validate path with `xizhi.ValidatePathAllowReserved(wsRoot, rel)` → 403 on escape; `os.Stat`: existing dir → 400 `BAD_REQUEST`.
- [x] 2.4 Reject NUL bytes in `content` via `isBinary([]byte(req.Content))` → 400 `BINARY_FILE`.
- [x] 2.5 `MkdirAll(filepath.Dir(abs), 0o755)`; call `atomicWriteFile(abs, []byte(req.Content), h.maxUploadBytes)`; map over-limit → 413 `FILE_TOO_LARGE`, write error → 500 `INTERNAL`. Return 200 `{path: relPath(...), size: len(content)}`.

## 3. Rename enhancements

- [x] 3.1 Extend `renameRequest` with `Overwrite bool \`json:"overwrite"\`` (keep `NewPath`).
- [x] 3.2 After resolving `dstAbs`, `os.Stat` it: if it is a directory, recompute `dstAbs = filepath.Join(dstAbs, filepath.Base(srcAbs))` (move-into-folder); preserve the existing `MkdirAll(filepath.Dir(dstAbs))` for the non-directory case.
- [x] 3.3 Replace the hard 409 existence check with: if final dst exists and `!Overwrite` → 409 `ALREADY_EXISTS`; if exists and `Overwrite` and dst is a directory → 409 `DEST_NOT_EMPTY`; otherwise (file dst, overwrite or absent) proceed.
- [x] 3.4 Keep root-rename guard (400), source-missing (404), and path-escape (403) behavior unchanged; ensure response still returns `{old_path, new_path}` against the *final* destination.

## 4. Routing & wiring

- [x] 4.1 Add `WorkspaceWriteContent gin.HandlerFunc` to `RouteDeps` in `internal/handler/router.go` (with a doc comment matching the others).
- [x] 4.2 Add `dispatchWorkspacePut(deps)` mirroring `dispatchWorkspaceFile`: if captured path ends with `contentRouteSuffix`, strip the suffix and forward to `WorkspaceWriteContent`; else forward to `WorkspaceRename`. Update the `RegisterAPIRoutes` PUT line to `authed.PUT("/workspace/files/*path", dispatchWorkspacePut(deps))`.
- [x] 4.3 Wire `WorkspaceWriteContent: h.WriteContent` in `cmd/blowball/serve.go` `wireAPI` (api role) next to the other workspace handlers; confirm the `all`/integration harness wiring also sets it.
- [x] 4.4 Update `test/integration/harness_test.go` (and any other `RouteDeps` builder) to set the new field so the suite compiles.

## 5. OpenAPI contract

- [x] 5.1 Add `PUT /api/v1/workspace/files/{path}/content` operation to `api/openapi.yaml` with `WriteContentRequest` (`content`, required) and `FileContentWriteResponse` (`path`, `size`); document 400 `BINARY_FILE`, 400 dir, 403, 413, 401.
- [x] 5.2 Add optional `overwrite` (boolean, default false) to `RenameRequest`; document the 409 `DEST_NOT_EMPTY` response and the move-into-folder behavior in the `PUT .../{path}` operation description.
- [x] 5.3 Copy `api/openapi.yaml` into `blowball-frontend/` and run `npm run generate-api` there to regenerate `src/lib/openapi.d.ts` (per repo convention).

## 6. Tests

- [x] 6.1 `WriteContent`: create-new (200, file on disk + parent auto-created), atomic-overwrite existing (old content fully replaced), NUL-byte → 400 `BINARY_FILE`, over-limit → 413, path-escape → 403, existing-directory target → 400, missing-auth → 401.
- [x] 6.2 `WriteContent`: verify atomicity expectation — on simulated write failure the original file is untouched (inject failure by writing into a read-only path or exceeding limit after a pre-existing file exists).
- [x] 6.3 `Rename`: move-into-folder (file → existing dir becomes `dir/<basename>`), move-directory-into-folder, `overwrite:true` replaces existing file (atomic), `overwrite:true` on existing directory → 409 `DEST_NOT_EMPTY`, `overwrite` absent on existing file → 409 `ALREADY_EXISTS` (regression), root rename → 400, source missing → 404.
- [x] 6.4 Add a production-routing test (like `TestTokenDownload_ProductionRouting`) asserting the PUT catch-all dispatches `.../content` to `WriteContent` and bare `.../*path` to `Rename`.

## 7. Docs & verification

- [x] 7.1 Update `CLAUDE.md` workspace route table: add `PUT /api/v1/workspace/files/*path/content` row and note the optional `overwrite` field on rename; mention move-into-folder semantics.
- [x] 7.2 `make build`, `make lint`, `make test` (incl. `go test ./internal/handler/...` and `go test ./test/integration/...`); confirm green.
