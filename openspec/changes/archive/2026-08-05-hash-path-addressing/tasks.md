## 1. Logical-name validation & hashing (sanitizer)

- [x] 1.1 Replace single-segment filename validation in `internal/minio/sanitizer.go` with logical-path validation: allow `/`; reject empty names, empty segments (`a//b`), `.`/`..` segments, NUL/C0 control characters; enforce a total length cap (Requirement: Files are addressed by logical path and hashed to a storage key)
- [x] 1.2 Add a pure hashing helper: `leaf = hex(sha256(logicalNameUTF8)) + ext` where `ext` is the original extension preserved verbatim; deterministic, no normalization (Requirement: Files are addressed by logical path and hashed to a storage key)
- [x] 1.3 Update key construction so `ObjectKey(uuid, logicalName)` produces `{uuid}/{<sha256-hex>}{<ext>}` (Requirement: Files are addressed by logical path and hashed to a storage key)

## 2. Storage layer

- [x] 2.1 Route `Upload`, `DownloadLatest`, `DownloadVersion`, `ListVersions`, and `Delete` through the hashing/key-construction path so they operate on the logical path's derived key (Requirements: Upload a file as a new version; List version history; Download the latest version; Download a specific historical version; Delete a file and all its versions)
- [x] 2.2 Remove the `ListFiles` storage method and its `ListObjects`-with-delimiter implementation (Requirement REMOVED: List files in a UUID directory)
- [x] 2.3 Leave `ListDirectories` unchanged (top-level prefix enumeration still valid)

## 3. HTTP handler layer

- [x] 3.1 Simplify `splitFilepath`: the whole `*filepath` (leading `/` trimmed) is the logical name; drop all action-suffix parsing
- [x] 3.2 Add `?action=` query dispatch: `version` / `versions` / `rollback` / none; plain POST = upload, `?action=rollback` = rollback (Requirements: Upload; List version history; Download the latest version; Download a specific historical version)
- [x] 3.3 Update the rollback handler to trigger on `?action=rollback`; keep target-version resolution from JSON body and/or `?versionId=` (version-rollback capability unchanged)
- [x] 3.4 Remove the `ListFiles` handler, its request/response models, and the `GET /documents/:uuid` route binding (Requirement REMOVED: List files in a UUID directory)

## 4. Router

- [x] 4.1 Keep `/documents/:uuid/*filepath` for GET/POST/DELETE; verify Gin captures real slashes in the catch-all with no `%2F` decoding dependency
- [x] 4.2 Remove the `GET /documents/:uuid` (ListFiles) route; keep `GET /documents` (ListDirectories) and `GET /healthz`

## 5. Tests

- [x] 5.1 Sanitizer unit tests: hashing determinism; extension preserved; same base name with different extensions does not collide; rejection of empty / empty-segment / `.` / `..` / control-char names; no-normalization behavior (Requirement: Files are addressed by logical path and hashed to a storage key)
- [x] 5.2 Handler unit tests for upload / download latest / download version / list versions / delete / rollback all addressed by logical path via `?action=`; include a logical path containing a segment literally named `versions` or `rollback` to prove no parsing ambiguity
- [x] 5.3 Remove obsolete `ListFiles` handler and storage tests
- [x] 5.4 Integration tests: logical-path upload creates new versions at the hashed key; download latest/specific round-trip; cross-UUID isolation; delete clears all versions; rollback unchanged
- [x] 5.5 Canonicalization round-trip test: upload via logical path P then download via the byte-identical P resolves to the same object (Requirements: Upload; Download the latest version; Files are addressed by logical path and hashed to a storage key)

## 6. Documentation

- [x] 6.1 Rewrite the README HTTP API section: logical-path addressing model, the `?action=` dispatch table, the key-derivation note, and the removal of `GET /documents/:uuid`; mark the changes as breaking
- [x] 6.2 Update README curl examples and the project-layout notes to match the new route shape and the dropped endpoint
- [x] 6.3 Rewrite `docs/openapi.yaml` for the v2 contract: collapse file operations onto `/documents/{uuid}/{filepath}` with `?action=` query dispatch and multi-content-type responses; rename `Filename`→`Filepath` (logical path, max 1024, no `/`-forbidding pattern); move `VersionId` to `query`; rename `VersionsResponse.filename`→`path`; remove the `GET /documents/{uuid}` operation and the `FilesResponse`/`FileEntry` schemas; update `info` to v2.0.0
