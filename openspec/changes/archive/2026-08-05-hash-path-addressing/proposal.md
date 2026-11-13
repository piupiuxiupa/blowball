## Why

External systems naturally organize files into logical directory hierarchies
(e.g. `财务/2024/年报.docx`), but the current API forces every file into a single
flat filename segment and rejects any `/`. We want clients to address files by
their natural logical path while keeping storage flat, safe, and decoupled from
human-readable names. Hashing each logical path to a single storage key delivers
normalization, collision-safety, and a degree of name obfuscation in one stroke —
without disturbing the versioning model, because the hash is deterministic on the
name (never the content), so the same logical path always maps to the same object
key and continues to accumulate versions.

## What Changes

- Files are addressed by a **logical path** that may contain `/`. The server
  hashes the full logical name **including its extension** with SHA-256 and
  stores it under the flat key `{uuid}/{<sha256-hex>}{<ext>}`. Version semantics
  are preserved because the hash is name-based, not content-based.
- **BREAKING** — Route refactor: operations beyond plain upload / download /
  delete move out of the URL path into an explicit `?action=` query parameter
  (`?action=versions`, `?action=version&versionId=<vid>`, `?action=rollback`).
  This eliminates the reserved-name ambiguity (the literal segments `versions`
  and `rollback` could otherwise collide with real directory names) once logical
  names are allowed to contain `/`.
- **BREAKING** — The `ListFiles` endpoint (`GET /documents/:uuid`) is removed.
  Server-side directory browsing is no longer possible because logical
  directories do not exist in storage — every file is a flat hash under
  `{uuid}/`. Clients track their own logical paths.
- Filename validation changes: the no-`/` rule is lifted for the logical name;
  instead the logical name is validated as a safe path (no `..` traversal, no
  empty segments, no control characters) and then hashed.
- Storage layer is unchanged in mechanism: still a single versioned bucket with
  `{uuid}/...` keys relying on native MinIO Object Versioning.

## Capabilities

### New Capabilities
<!-- None — this reshapes the existing storage capability's addressing model. -->

### Modified Capabilities
- `versioned-file-storage`: addressing changes from a single-segment filename to
  a logical path that is hashed (SHA-256, full hex, extension preserved) into the
  storage key; the "List files in a UUID directory" requirement is removed;
  filename/path validation rules change accordingly; upload, download, list
  versions, and delete now operate on the logical path.

## Impact

- **Public API (BREAKING)**: `/documents/...` routes change shape — path-based
  logical addressing, query-based actions, and removal of `GET /documents/:uuid`.
  The README API section, all examples, and every client must be updated.
- **Code**: `internal/minio/sanitizer.go` (logical-name validation + hashing;
  drop the no-`/` rule), `internal/minio/storage.go` (hash before key
  construction; remove `ListFiles`), `internal/handler` (`splitFilepath`
  simplified to "whole path is the logical name"; dispatch via method + `?action=`),
  `internal/router/router.go` (unchanged route shape, shifted semantics).
- **Storage key shape**: changes from `{uuid}/{filename}` to
  `{uuid}/{<sha256-hex>}{<ext>}`. Data written under the old verbatim key shape is
  **not** reachable through the new API (a verbatim filename no longer hash-matches
  its old key). See design migration plan.
- **Dependencies**: none added or removed.
- **Tests**: handler, sanitizer, and integration tests all need updates for the
  new addressing and route model; `ListFiles` tests are removed.
