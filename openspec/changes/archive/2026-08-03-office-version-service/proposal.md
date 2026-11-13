## Why

External systems produce and revise Office files (.docx/.xlsx/.pptx) and need a reliable way to keep full history, view/download past versions, and roll back to a prior version when something goes wrong — without standing up a database or building diff/merge logic. MinIO's native S3 Object Versioning already preserves every version of an object; this service exposes that capability through a simple HTTP API, using a caller-supplied directory UUID to namespace each external system's files.

## What Changes

- New Go + Gin HTTP service exposing a REST API for Office (and arbitrary) file versioning.
- Upload files under a caller-supplied directory UUID; files stored at `{uuid}/{filename}` keys in a single MinIO bucket with native Object Versioning enabled.
- List the version history of a file (VersionId, timestamp, size, isLatest).
- Download the latest version, or a specific historical version.
- Non-destructive rollback: promote a prior version's content to become the new latest version (full history preserved).
- List files within a UUID directory; list UUID directories.
- Delete a file (removes all of its versions).
- No authentication for MVP; the service is stateless and MinIO is the sole source of truth.

## Capabilities

### New Capabilities
- `versioned-file-storage`: upload files into a UUID-namespaced directory, track versions via MinIO native Object Versioning, list version history, download the latest or a specific version, list directory contents, and delete files.
- `version-rollback`: non-destructively restore a prior version of a file as the new latest version without discarding any history.

### Modified Capabilities
<!-- Greenfield project — no existing specs to modify. -->

## Impact

- **New codebase**: a Go module with Gin HTTP handlers, a MinIO client wrapper, configuration (endpoint / access key / secret / bucket name), and bootstrap logic that ensures the target bucket exists and has Object Versioning enabled.
- **New runtime dependency**: a MinIO instance (or any S3-compatible object store).
- **New public HTTP API** under `/documents/...`.
- **No database** and **no auth layer** for the MVP.
