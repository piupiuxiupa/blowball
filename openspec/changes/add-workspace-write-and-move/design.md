## Context

The workspace REST layer (`internal/handler/workspace.go`) can read a file's text content via `GET .../files/*path/content` (handler `Content`), but the only write paths are:

1. `POST .../upload` — multipart, which writes the destination with `c.SaveUploadedFile` (opens `O_WRONLY|O_CREATE|O_TRUNC`). It overwrites a same-named file but is **non-atomic** (a crash leaves a truncated file) and **filename-coupled** (the destination name is always the uploaded part's `Filename`).
2. The OnlyOffice callback (`onlyOfficePersist`) — atomic temp-then-`os.Rename`, but only for office documents.

The move/rename endpoint `PUT .../files/*path` (handler `Rename`) already does cross-directory moves via `os.Rename` + `MkdirAll` of the destination parent, but hard-codes a 409 whenever the destination already exists and has no notion of "move *into* a directory". Routing today: GET uses a suffix dispatcher (`dispatchWorkspaceFile`) to split `download/`/`content`/bare; DELETE and PUT are registered directly on the same `/*path` catch-all under different HTTP methods.

Constraints: gin forbids a static sibling route alongside a `/*path` wildcard at the same tree node, so a new `PUT .../content` cannot be registered as a separate route — it must be dispatched by suffix like GET already does. All workspace path validation goes through `xizhi.ValidatePathAllowReserved` on the REST side (the `.blowball/` namespace is reserved only for the agent's `xizhi_*` tools). The per-user workspace root is `data/{userID}/workspace`; in `storage.workspace.backend: shared` mode it lives on a single JuiceFS mount (so `os.Rename` never crosses devices).

## Goals / Non-Goals

**Goals:**
- Provide an atomic, text-only, create-or-replace write endpoint symmetric to `GET .../content`, so the frontend can round-trip edited text without multipart and without crash-window corruption.
- Let users reorganize their workspace naturally: "drag into a folder" (destination is an existing directory) and "replace on move" (optional `overwrite`), without breaking the default 409-on-existing-file contract.
- Reuse the existing atomic-write pattern and routing/dispatch conventions; no new top-level route shape, no new dependencies, no DB/Redis involvement.

**Non-Goals:**
- Fixing upload atomicity or filename coupling — upload is left as-is; the new content-write endpoint and OnlyOffice are the atomic paths.
- Optimistic concurrency / conflict detection on writes (no `expected_mtime`/`if-match`). Last writer wins. Can be added later if the agent-vs-user write race proves painful.
- Partial edits / append / bytestream write. Whole-file replace only (mirrors `xizhi_write_file`).
- Batch move (multiple sources in one request). One source per request.
- Directory merge on overwrite (recursively merging two trees). Rejected, not implemented.

## Decisions

### D1: Route the new endpoint as `PUT .../files/*path/content` via PUT suffix dispatch
**Choice:** Add a PUT-side dispatcher parallel to `dispatchWorkspaceFile`: when the captured `*path` ends with `/content` (the existing `contentRouteSuffix`), route to `WriteContent`; otherwise fall through to `Rename`. The existing `authed.PUT("/workspace/files/*path", deps.WorkspaceRename)` registration is replaced by `authed.PUT("/workspace/files/*path", dispatchWorkspacePut(deps))`.

**Why:** gin rejects registering a static `/content` sibling next to a `/*path` wildcard at the same node, so a separate route is impossible. The GET side already solved this exactly — `dispatchWorkspaceFile` suffix-routes `/content`, `/download`, and bare — so PUT gains the identical mechanism, the `/content` URL is already familiar to clients from the GET side, and bare `PUT .../*path` keeps its existing rename semantics.

**Alternatives considered:**
- *`PATCH .../content` (different verb on its own catch-all):* avoids touching the PUT dispatcher, but introduces a second workspace catch-all verb and a less idiomatic method for create-or-replace. PUT matches the create-or-replace semantics and mirrors GET.
- *A distinct URL shape (`POST .../files/*path/write`):* diverges from the established `/content` suffix pairing and adds a new shape rather than reusing one.

### D2: Atomic create-or-replace via temp-file + `os.Rename` (extract a shared helper)
**Choice:** `WriteContent` writes the body to a temp file created with `os.CreateTemp(filepath.Dir(abs), ".ws-write-*")`, then `os.Rename`s it over the target on success; on any error it removes the temp and leaves the original untouched. Factor this into a small helper (e.g. `atomicWriteFile(abs string, data []byte, limit int64) (int, error)`) and refactor `onlyOfficePersist` to use the same primitive, removing duplication.

**Why:** This is the exact pattern `onlyOfficePersist` already uses and trusts for office saves; reusing it gives the content-write endpoint the same crash-safety for free and removes a duplicated temp/rename/limit recipe. `CreateTemp` in the target directory guarantees the temp and final path share a filesystem, so `os.Rename` is atomic (no `EXDEV`).

**Alternatives considered:**
- *Direct `os.WriteFile(abs, ...)` (like `xizhi_write_file`/`xizhi_modify_file`):* simpler, but non-atomic — a crash mid-write truncates the file. Unacceptable for a user-facing edit endpoint that may replace large text the user just typed.
- *Write-then-`fsync`-then-rename:* stronger durability; deferred — `os.Rename` atomicity is the property we need for correctness, and matching `onlyOfficePersist` keeps the two paths consistent.

### D3: Create-or-replace semantics (PUT)
**Choice:** A missing target file is created (with `MkdirAll` for missing parents); an existing file is replaced. The handler does **not** 404 on a missing file.

**Why:** HTTP PUT is idempotent create-or-replace by definition, and this matches `xizhi_write_file` (which the agent already uses to create files). Making the endpoint create-or-replace means the frontend's "save" button works uniformly for both new and existing files, and it is a strict superset of "modify".

**Alternatives considered:**
- *404 if missing (pure "modify"):* forces the client to choose between upload (create) and modify (existing) and re-introduces the non-atomic upload path for creation. Strictly worse for the editor UX.

### D4: Text-only — reject NUL bytes with 400 `BINARY_FILE`
**Choice:** After JSON-decoding `content`, scan for a NUL byte (the existing `isBinary` heuristic already detects this); if present, reject with 400 `BINARY_FILE` and do not touch the file.

**Why:** Symmetry with the read side — `GET .../content` refuses to return binary (`isBinary` → 400 `BINARY_FILE`), so allowing a NUL-bearing write would create a file the read endpoint could not serve back. Binary/large content keeps using `POST .../upload`. The check is cheap (scan the in-memory string once).

### D5: Move-into-folder when `new_path` is an existing directory
**Choice:** In `Rename`, after resolving `dstAbs`, `os.Stat` it: if it is a directory, recompute `dstAbs = filepath.Join(dstAbs, filepath.Base(srcAbs))` and continue. The existence/overwrite check then runs against this recomputed destination.

**Why:** This is the single most natural "reorganize my files" gesture (select file → drop into folder). Today it returns 409 because the directory "exists", which is a limitation rather than intended protection. Computing `dst/<basename>` matches POSIX-shell `mv src dst/` semantics and what users expect from a file picker.

**Edge case:** if `dst/<basename>` itself already exists, the normal existence/overwrite logic applies (409 unless `overwrite`). No special-casing needed.

### D6: Optional `overwrite` flag; file-only, directory-rejected
**Choice:** Extend `renameRequest` with `Overwrite bool`. The pre-flight existence check becomes: if the final destination exists, then unless `Overwrite` is true, 409 `ALREADY_EXISTS` (unchanged); if `Overwrite` is true and the destination is a **file**, proceed to `os.Rename` (which atomically replaces the file); if `Overwrite` is true and the destination is a **directory**, return 409 `DEST_NOT_EMPTY` (do not attempt the rename).

**Why:** `os.Rename` atomically replaces an existing *file* on POSIX for free, so overwrite-a-file is safe and atomic. Replacing directories is a tarpit of `ENOTEMPTY`/`EISDIR`/`ENOTDIR` and would amount to tree-merge, which is explicitly out of scope (Non-Goal). Branching on `dst.IsDir()` before the rename yields a clear, testable `DEST_NOT_EMPTY` instead of a generic 500.

**Alternatives considered:**
- *Always allow `os.Rename` and map its error:* leaks opaque POSIX errors as 500s and offers no clean contract. Rejected.
- *Recursive merge when destination is a directory:* scope creep, surprising data movement. Deferred.

### D7: Cap the write body at `maxUploadBytes`; response is `{path, size}`
**Choice:** Apply `http.MaxBytesReader` to the request body before JSON decode (same as `Upload`), and additionally guard `len(content) <= maxUploadBytes`. Respond with `{"path": <rel>, "size": <len>}` — matching `UploadResponse`, not echoing `content`.

**Why:** Reuses the existing per-file cap so the write endpoint cannot be used to bypass upload limits. Echoing `content` would double the payload for no client benefit; `{path, size}` matches the upload contract and is enough for the UI to refresh.

### D8: Wiring & roles
**Choice:** Add `WorkspaceWriteContent` to `RouteDeps` and wire it in `wireAPI` (api role) alongside the other workspace CRUD handlers; the `all` role picks it up via the shared wiring. The PUT dispatcher sits under the existing `authed` group, so it inherits `AuthMiddleware` exactly like the current direct `WorkspaceRename` registration — no auth change.

**Why:** Content write and rename are CRUD, not agent execution; they belong in the api role with no dependency on the orchestrator/OpenAI/tool-registry. Keeping auth on the `authed` group (not a new query-token path) matches the existing `PUT`/`DELETE` workspace routes.

## Risks / Trade-offs

- **[Behavior change: move-into-folder turns a former 409 into success]** → A client that relied on 409 to detect "destination is a directory" will see different behavior. Mitigation: documented in the proposal and spec; the new behavior is the strictly more useful one and matches `mv` semantics. Low likelihood of real dependence.
- **[Upload remains non-atomic]** → Out of scope here; the new content-write endpoint and OnlyOffice cover the atomic paths. Documented as a Non-Goal so it is not assumed fixed.
- **[`os.Rename` `EXDEV` across filesystems]** → Cannot happen in practice: per-user workspace roots are a single mount (local dir or one JuiceFS mount in `shared` mode), and temp files are created in the target's own directory. Not a runtime concern; noted for completeness.
- **[Overwrite replaces a file with no archive]** → Consistent with `DELETE` (workspace files have no DB source table and are not archived). The replaced bytes are unrecoverable, same as a delete-then-upload. Acceptable and consistent.
- **[Suffix-dispatch false positive: a path whose final segment is literally `content`]** → A file named `content` (no extension) at the workspace root would match the `/content` suffix and be routed to `WriteContent` instead of `Rename`. This is the identical ambiguity the GET side already lives with (`dispatchWorkspaceFile`), so precedent exists and behavior is consistent across verbs; the risk is limited to an unusual filename.
- **[Agent-vs-user concurrent write race]** → With create-or-replace + last-writer-wins, a user edit can silently clobber an in-flight agent `xizhi_write_file`/`xizhi_modify_file` on the same file, and vice versa. Accepted for v1 (Non-Goal: conflict detection); the atomic write at least guarantees neither side sees a torn file.

## Migration Plan

- **Deploy:** purely additive — one new route (`PUT .../content`), one optional body field (`overwrite`). No schema, config, or data migration. Ship behind the normal release; clients adopt the new operations as needed.
- **Rollback:** revert the code change; no persistent state to undo. Existing files written via the new endpoint are ordinary workspace files and need no cleanup.
- **Communication:** note the one observable behavior change (rename into an existing directory now succeeds instead of 409) in the release notes for API consumers.

## Open Questions

- **Spec drift: `xizhi.ValidatePath` vs `ValidatePathAllowReserved`.** The `workspace-file-rename` spec text says paths are validated by `xizhi.ValidatePath`, but the REST code (and this change) uses `ValidatePathAllowReserved` so users can manage their own `.blowball/` state. This delta preserves the existing spec wording to stay surgical; reconciling the doc with the code is a separate cleanup.
- **Should `WriteContent` echo `content` in the response?** Currently chosen as `{path, size}` only. Confirm with the frontend (blowball-frontend) that no client expects the echoed body.
- **Future: optimistic concurrency** (`expected_mtime` / `If-Match`) to guard the agent-vs-user write race. Defer until there is evidence of real collisions.
