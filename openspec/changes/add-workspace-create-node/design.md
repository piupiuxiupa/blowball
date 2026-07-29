## Context

The workspace REST layer (`internal/handler/workspace.go`) can already:

- create a **file with text content** atomically via `PUT .../files/*path/content` (`WriteContent`, added in `add-workspace-write-and-move`) — create-or-replace, temp-file + `os.Rename`;
- create a **file via multipart** via `POST .../upload` (`Upload`) — non-atomic `SaveUploadedFile`, filename taken from the uploaded part;
- rename/move (`PUT .../*path`, `Rename`, with move-into-folder + optional `overwrite`), delete (`DELETE .../*path`), and read (download / `GET .../content`).

What is missing is any operation whose **target is a directory**: the three `os.MkdirAll` calls in the handler (`Upload` L182, `WriteContent` L436, `Rename` L564) all create *parent* directories of a file operation. There is no way to create an **empty folder** — the most basic file-manager gesture — nor an explicit "create new empty file" primitive distinct from "save content".

Routing today: a single catch-all `/*path` on `/workspace/files` is shared across verbs. GET dispatches by suffix (`/content`, `/download`, `/onlyoffice-config`) via `dispatchWorkspaceFile`; PUT dispatches by suffix (`/content` → write, else rename) via `dispatchWorkspacePut`; DELETE is registered directly. **POST on the catch-all is unused** — the only workspace POST is the static segment `/workspace/upload`, which lives at a different tree node and does not collide with `/workspace/files/*path`.

Constraints: gin forbids a static sibling route alongside a `/*path` wildcard at the same tree node, so any new operation on the catch-all must either register under a free verb or dispatch by suffix. All workspace path validation on the REST side goes through `xizhi.ValidatePathAllowReserved` (the `.blowball/` namespace is reserved only for the agent's `xizhi_*` tools, which use the stricter `xizhi.ValidatePath`). The per-user workspace root is `data/{userID}/workspace`; in `storage.workspace.backend: shared` mode it lives on a single JuiceFS mount.

## Goals / Non-Goals

**Goals:**
- Let the frontend create an **empty directory** ("new folder") and an **empty file** ("new file") through one unified, strict create endpoint.
- Make "already exists" behavior **identical** for files and directories (409 `ALREADY_EXISTS`), so the frontend does not branch on node type.
- Keep the create race-free and atomic by mapping strict-create directly to `O_EXCL` / `os.Mkdir`, avoiding any check-then-create window.
- Auto-create missing parent directories (leaf-strict + auto-parents), consistent with the existing `WriteContent`/`Upload` parent-creation behavior.
- Reuse the existing path validator, routing slot, and wiring conventions; no new top-level route shape, no suffix dispatcher, no DB/Redis involvement, no new dependencies.

**Non-Goals:**
- Writing content as part of create. `PUT .../content` remains the content path (create-or-replace); the new endpoint creates **empty** nodes only. The two are intentionally distinct (strict vs. replace).
- Idempotent / `mkdir -p`-on-the-leaf semantics. An existing leaf is an error (409), not a no-op.
- Recursive tree creation, copy, or tree merge. One leaf node per request.
- Optimistic concurrency / conflict detection (no `If-Match`/`expected_mtime`). Strict-create's `EEXIST` is the only conflict signal.
- Surfacing POSIX errno verbatim; errors map to the existing `errorBody` codes.

## Decisions

### D1: One unified `POST .../files/*path` endpoint with a body `type`
**Choice:** A single `WorkspaceHandler.Create` handles both node kinds; the request body `{"type": "file" | "directory"}` selects which. The path comes from the URL catch-all, matching `Rename` (params in the body, identity in the URL).

**Why:** A file manager's "new folder" and "new file" are the same gesture with a different target kind; one endpoint with a discriminator avoids a near-duplicate `mkdir`/`touch` pair and keeps the response shape (`{path, type}`) uniform. PUT is already taken on the catch-all (rename + write-content), and POST is free, so `POST .../*path` is the natural verb for "create".

**Alternatives considered:**
- *Two endpoints (`POST .../mkdir`, `POST .../touch`):* doubles the routing/wiring/test surface for two operations that differ by one syscall; rejected.
- *A `type` query param instead of a body:* a body is consistent with `renameRequest` and leaves room for future fields (e.g. optional initial content later); the empty-body-with-query form is less idiomatic here.

### D2: Strict create via `O_EXCL` (file) and `os.Mkdir` (directory) — no check-then-create
**Choice:** File: `os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)`. Directory: `os.Mkdir(abs, 0o755)`. Both return `EEXIST` when the leaf already exists, mapped to 409 `ALREADY_EXISTS`.

**Why:** `O_EXCL`/`Mkdir` make "does not already exist" and "create it" a single atomic step, so two concurrent creates of the same path resolve cleanly (one 200, one 409) with no TOCTOU window. This is simpler and safer than the rename change's stat-then-act flow, and it gives files and directories a symmetric, testable existence contract.

**Alternatives considered:**
- *Check-then-create (`os.Stat` then `os.Create`/`MkdirAll`):* introduces a race and a branch; strictly worse. Rejected.
- *`os.MkdirAll` for directories (idempotent on the leaf):* would turn "exists" into a silent 200, contradicting the strict decision and diverging from the file branch. Rejected.

### D3: Leaf-strict + auto-create parents (option A)
**Choice:** Before the strict leaf create, call `os.MkdirAll(filepath.Dir(abs), 0o755)` to establish any missing parents. The strict existence guarantee applies only to the **leaf** (`abs` itself).

**Why:** Consistent with `WriteContent` and `Upload`, which both `MkdirAll` the parent without complaint, so a nested create (`a/b/c`) succeeds even when `a/b/` does not yet exist. "Strict" is preserved where it matters — you cannot clobber an existing leaf — while the friendlier file-manager UX (deep paths just work) is kept. `MkdirAll` on the parent is idempotent and never touches the leaf, so it cannot weaken leaf-strictness.

**Alternatives considered:**
- *Full POSIX strict (`mkdir` without `-p`, parent must exist):* more "hardcore", but it diverges from the existing write endpoints' auto-parent behavior and surfaces surprising "parent missing" errors in a file-manager UI. Rejected.

### D4: Identical 409 `ALREADY_EXISTS` for files and directories
**Choice:** Whether the existing leaf is a file or a directory, the response is 409 `ALREADY_EXISTS`. No special-casing on the existing node's type.

**Why:** The create contract is "the target must not exist"; the *kind* of the existing node is irrelevant to that contract, and uniform behavior means the frontend handles a single error path. This also sidesteps the `EEXIST`-on-file-vs-directory ambiguity entirely.

**Alternatives considered:**
- *409 `ALREADY_EXISTS` for a file but a different code for a directory:* adds a distinction no caller needs and complicates the OpenAPI contract. Rejected.

### D5: Register directly on the POST catch-all — no suffix dispatch
**Choice:** `authed.POST("/workspace/files/*path", deps.WorkspaceCreate)` in `RegisterAPIRoutes`, with no dispatcher wrapper.

**Why:** Unlike GET (`/content`, `/download`, `/onlyoffice-config`) and PUT (`/content`), POST has only one operation on the catch-all, so there is nothing to dispatch on. The catch-all POST slot is unused today (`/workspace/upload` is a separate static route), so registration is conflict-free and the handler reads the full `*path` directly.

**Alternatives considered:**
- *A suffix dispatcher mirroring GET/PUT:* introduces machinery that dispatches nothing; rejected as needless complexity.

### D6: Validation — `ValidatePathAllowReserved`, root create → 400, `type` required
**Choice:** Validate the path with `xizhi.ValidatePathAllowReserved(wsRoot, rel)` → 403 on escape (identical to the other REST workspace handlers, so users can manage their own `.blowball/`). If the trimmed path is empty (i.e. the user is trying to "create" the workspace root itself), return 400 `BAD_REQUEST`. Require `type` to be exactly `"file"` or `"directory"`; anything else (including missing) → 400 `BAD_REQUEST`.

**Why:** Path-validation parity keeps the REST surface coherent and reuses the established escape/symlink defense. The root is structurally created at user-dir setup time, so "create root" is meaningless and is rejected up front rather than producing a confusing `EEXIST`/`EISDIR`. A closed `type` enum makes the contract explicit and gives a clean 400 instead of a default kind guess.

**Alternatives considered:**
- *Default `type` to `file` when omitted:* hides client bugs and makes "I forgot the body" silently create files; a required enum is safer.
- *Reject the `.blowball/` namespace like the agent path:* would break parity with the other REST handlers and prevent users from organizing their own skill state. Rejected.

### D7: Response `{path, type}`; no content echo
**Choice:** On success return `200 {"path": <relPath>, "type": <"file"|"directory">}`. Do not echo any body field beyond `type`.

**Why:** `path` lets the UI refresh/locate the new node; `type` confirms what was created (useful since it is a request input). The created node is empty, so there is no size or content worth returning. The shape parallels the other workspace responses (`{path, size}` for content/upload, `{old_path, new_path}` for rename).

### D8: Wiring & roles
**Choice:** Add `WorkspaceCreate` to `RouteDeps` and wire it in `wireAPI` (api role) next to the other workspace CRUD handlers; the `all` role and integration harness pick it up via the shared wiring. The route sits under the existing `authed` group, inheriting `AuthMiddleware` exactly like the current workspace routes.

**Why:** Create is CRUD, not agent execution; it belongs in the api role with no dependency on the orchestrator/OpenAI/tool-registry, matching the boundary already enforced for `WriteContent`/`Rename`/`Delete`.

## Risks / Trade-offs

- **[Two ways to create a file: `POST .../*path` (strict) vs `PUT .../content` (replace)]** → They are intentionally different operations with different contracts. Mitigation: documented explicitly in the proposal and spec; the frontend uses create for "new node" and write-content for "save editor contents".
- **[Agent-vs-user create race]** → An agent's `xizhi_write_file`/`xizhi_modify_file` (create-or-replace) can clobber or be clobbered by a user's strict create on the same path within the FS race window; strict-create's `EEXIST` only protects against a *pre-existing* node, not a concurrent writer. Accepted for v1 (Non-Goal: concurrency control); the per-user workspace is single-user in practice.
- **[`O_EXCL` does not atomically create parents + leaf in one syscall]** → `MkdirAll(parent)` then `O_EXCL`/`Mkdir(leaf)` is two steps; a concurrent delete of the parent between them is a pathological race. Mitigation: low likelihood (single-user workspace), and the failure surfaces as a normal 500 `INTERNAL` mapped from the syscall error; no silent corruption.
- **[Reserved-name collision (`/content`, `/download`)]** → Not applicable to POST: there is no suffix dispatch on POST, so a directory literally named `content` is created/identified by its full path with no routing ambiguity (unlike the GET/PUT sides, which already live with this).
- **[Overwrite-a-directory via create]** → Cannot happen: an existing directory leaf returns 409 `ALREADY_EXISTS` and is never touched; there is no `overwrite` flag on create.

## Migration Plan

- **Deploy:** purely additive — one new route (`POST .../files/*path`), one new request schema (`CreateNodeRequest`). No schema, config, or data migration. Ship behind the normal release; clients adopt as needed.
- **Rollback:** revert the code change; no persistent state to undo. Any directories/files created via the new endpoint are ordinary workspace nodes and need no cleanup.
- **Communication:** none required beyond the OpenAPI addition — no existing endpoint or contract changes.

## Open Questions

- **Frontend scenario grounding.** The spec scenarios are written from the API contract; confirming the actual "new folder"/"new file" UI gestures in `blowball-frontend` would let the scenarios mirror real usage. Non-blocking — the contract is fully determined by the decisions above.
- **Future: optional initial content on create?** A `content` field on `CreateNodeRequest` would fold empty-file-create and write-content into one call, but at the cost of duplicating the atomic-write path and the NUL-byte/binary/size guards. Deferred; the two endpoints stay separate for v1.
