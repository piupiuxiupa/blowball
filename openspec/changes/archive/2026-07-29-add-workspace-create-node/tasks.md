# Tasks

## 1. CreateNode handler

- [x] 1.1 Add `createNodeRequest` (`Type string \`json:"type"\``) and `createNodeResponse` (`{Path string \`json:"path"\`; Type string \`json:"type"\`}`) types in `internal/handler/workspace.go`.
- [x] 1.2 Implement `WorkspaceHandler.Create`: resolve `userID := middleware.UserIDFromCtx(c)`, `tid`, `ctx`; `wsRoot := h.fsSvc.UserWorkspace(userID)`; `rel := strings.TrimPrefix(c.Param("path"), "/")`.
- [x] 1.3 Validate path with `xizhi.ValidatePathAllowReserved(wsRoot, rel)` → 403 `FORBIDDEN` ("path outside workspace") on escape; if `rel == ""` (create-root) → 400 `BAD_REQUEST`.
- [x] 1.4 `c.ShouldBindJSON(&req)`; require `req.Type` to be exactly `"file"` or `"directory"`, else 400 `BAD_REQUEST` (covers missing and invalid `type`).
- [x] 1.5 `os.MkdirAll(filepath.Dir(abs), 0o755)` to auto-create parents (option A, leaf-strict + auto-parents); map error → 500 `INTERNAL`.
- [x] 1.6 Branch on type: file → `os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)` then `f.Close()`; directory → `os.Mkdir(abs, 0o755)`. Map `errors.Is(err, os.ErrExist)` → 409 `ALREADY_EXISTS`; other errors → 500 `INTERNAL`. Return 200 `createNodeResponse{Path: relPath(wsRoot, abs), Type: req.Type}`.

## 2. Routing & wiring

- [x] 2.1 Add `WorkspaceCreate gin.HandlerFunc` to `RouteDeps` in `internal/handler/router.go` with a doc comment matching the other workspace fields.
- [x] 2.2 Register `authed.POST("/workspace/files/*path", deps.WorkspaceCreate)` in `RegisterAPIRoutes`; add the corresponding line to the `RegisterRoutes` route comment block. No suffix dispatcher (POST has no `/content` split).
- [x] 2.3 Wire `WorkspaceCreate: h.Create` in `cmd/blowball/serve.go` `wireAPI` (api role) next to the other workspace handlers; confirm the `all` role and integration harness wiring also set it.
- [x] 2.4 Update `test/integration/harness_test.go` (and any other `RouteDeps` builder) to set `WorkspaceCreate` so the suite compiles.

## 3. OpenAPI contract

- [x] 3.1 Add `POST /api/v1/workspace/files/{path}` operation to `api/openapi.yaml` with `CreateNodeRequest` (`type`, required, enum `["file", "directory"]`) and the `{path, type}` 200 response; document 400 (create-root / bad `type`), 403 `FORBIDDEN`, 409 `ALREADY_EXISTS`, 401.
- [ ] 3.2 Copy `api/openapi.yaml` into `blowball-frontend/` and run `npm run generate-api` there to regenerate `src/lib/openapi.d.ts` (per repo convention).
  - **Deferred (manual, cross-repo):** `blowball-frontend` is a separate sibling repo outside this change's `allowedEditRoots` (`/root/workspace/blowball`), so it was not edited here. The repo has the `generate-api` script (`openapi-typescript ./openapi.yaml -o src/lib/openapi.d.ts`) and `node_modules` installed. Run when ready: `cp api/openapi.yaml ../blowball-frontend/openapi.yaml && (cd ../blowball-frontend && npm run generate-api)`.

## 4. Tests

- [x] 4.1 `Create`: create file (200, empty file on disk), create directory (200, dir on disk), nested create auto-builds missing parents (200, `a/b/c` with `a/b` absent).
- [x] 4.2 `Create`: existing file → 409 `ALREADY_EXISTS` (original untouched); existing directory → 409 `ALREADY_EXISTS` (identical behavior); create-root (empty path) → 400 `BAD_REQUEST`.
- [x] 4.3 `Create`: missing `type` → 400; invalid `type` (e.g. `"folder"`) → 400; path-escape (`..`/absolute/symlink) → 403; missing auth → 401.
- [x] 4.4 Add a production-routing test (like `TestTokenDownload_ProductionRouting`) asserting `authed.POST("/workspace/files/*path")` routes to `WorkspaceCreate` and the catch-all does not collide with `POST /workspace/upload`.

## 5. Docs & verification

- [x] 5.1 Update `CLAUDE.md` workspace route table: add the `POST /api/v1/workspace/files/*path` row; note the body `{type}` and strict 409 `ALREADY_EXISTS` semantics (leaf-strict + auto-parents).
- [x] 5.2 `make build`, `make lint`, `make test` (incl. `go test ./internal/handler/...` and `go test ./test/integration/...`); confirm green.
  - `make build`, `make lint`, and `go vet ./...` are green. All new `TestCreate*` tests pass, the updated route-set test passes, and `go test ./test/integration/... -skip 'TestExecutor|TestApplyLandlock|TestProbeFUSE'` is green. The only failing tests anywhere are `TestExecutor*` / `TestApplyLandlock_*`, the pre-existing WSL2 bwrap/landlock environmental failures (documented non-regressions, unrelated to this change).
