## Context

Greenfield service. External systems revise Office files over time and need full version history, the ability to view/download past versions, and a rollback path. MinIO (S3-compatible) already implements Object Versioning at the bucket level — when enabled, every `PUT` to the same key produces a new `VersionId` instead of overwriting, and every historical version is retained and retrievable. This design deliberately leans on that native capability rather than building version logic in the application, so the service stays thin and stateless.

Tech stack: Go, Gin (`github.com/gin-gonic/gin`), MinIO SDK (`github.com/minio/minio-go/v7`).

## Goals / Non-Goals

**Goals:**
- Provide a small HTTP API to upload files, view version history, download any version, and roll back — for Office files and arbitrary binary blobs.
- Use MinIO's native Object Versioning as the sole mechanism for retaining history (no database, no application-level deltas).
- Namespace each external system's files under a caller-supplied directory UUID (`{uuid}/{filename}`).
- Keep the service stateless: MinIO is the single source of truth.
- Roll back non-destructively — history is never discarded.

**Non-Goals:**
- Authentication / authorization (MVP).
- Content diffing, merging, or structural comparison of Office (OOXML) content.
- Real-time co-authoring / concurrent-edit resolution.
- Git-style branches, tags, or DAG history.
- Storing rich commit metadata (author, message) beyond MinIO's native object metadata.
- Version count limits / retention automation in the MVP (deferred to S3 lifecycle rules).

## Decisions

### Decision 1: Native Object Versioning, not application-level versioning
**Choice:** Enable MinIO bucket Object Versioning and let every `PUT` auto-generate a new `VersionId`. History is read via `ListObjectVersions`; a specific version is read via `GetObject(versionId=...)`.

**Alternatives considered:**
- *Service-layer versioning with per-version keys* (`{uuid}/{filename}/v3`): full control, but reimplements what MinIO already does and loses the `isLatest`/`VersionId` conveniences.
- *Hybrid (MinIO blobs + a metadata DB for a commit graph):* required only for branches/authors/diff — all out of scope. Rejected as over-engineering for MVP.

**Rationale:** Native versioning is the cheapest correct solution for "keep every version, retrieve any version," which is the entire scope.

### Decision 2: Single versioned bucket, `{uuid}/{filename}` key prefix
**Choice:** One bucket with versioning on; each external system's files live under a `{uuid}/` prefix. Filenames are unique within a UUID.

**Alternatives considered:**
- *One bucket per UUID:* versioning is bucket-scoped and managing arbitrarily many buckets is operational overhead; rejected.
- *Flat keys with UUID embedded in the filename:* loses the natural "list files in this directory" operation that prefixes give us.

**Rationale:** A prefix namespace gives free directory semantics (`ListObjects` with prefix + delimiter) and isolates coincidentally identical filenames across UUIDs.

### Decision 3: Implicit directory creation
**Choice:** A UUID directory is not pre-created. It springs into existence when the first file is uploaded under `{uuid}/{filename}`. No explicit "create directory" endpoint or 0-byte folder marker.

**Alternatives considered:**
- *Explicit `{uuid}/` 0-byte marker object so empty directories are listable:* rejected unless a requirement emerges to enumerate UUIDs that have no files yet. A marker in a versioned bucket is itself a versioned object and adds noise.

**Rationale:** Simplest model; covers the stated flow (external system supplies UUID, then uploads files).

### Decision 4: Non-destructive rollback via server-side `CopyObject`
**Choice:** Rollback performs a server-side `CopyObject` from `{uuid}/{filename}` at `versionId=<target>` to the same key with no version constraint. This creates a **new** latest version whose content equals the target version. All prior versions (including those "after" the target) are retained.

**Alternatives considered:**
- *Destructive rollback (delete versions newer than the target):* loses history irreversibly; rejected because "undo must itself be undoable."
- *Client-side download-then-reupload:* works but wastes bandwidth; `CopyObject` is server-side.

**Rationale:** History-preserving rollback is the safer default and matches the Dropbox/Google-Drive model users expect.

### Decision 5: Stateless service, no database
**Choice:** All state lives in MinIO. The service holds only configuration and a MinIO client. No DB.

**Rationale:** MinIO native versioning already stores everything needed (VersionId, timestamps, size, isLatest). A DB would add operational surface for no MVP benefit.

### Decision 6: Bootstrap ensures bucket + versioning
**Choice:** On startup the service checks that the configured bucket exists and has versioning enabled; if missing it creates the bucket and enables versioning. The service refuses to start (or fails fast) if versioning cannot be enabled.

**Rationale:** Versioning must be enabled **before** any object is written; versioning cannot retroactively recover versions written before it was on.

## Risks / Trade-offs

- **Unbounded version growth** → Office files are large; versions accumulate forever. *Mitigation:* add an S3 lifecycle rule (non-current version expiration / N-days) later; zero code change. Out of scope for MVP.
- **No auth = open service** → Any network client can read/write/delete. *Mitigation:* deploy on a private network / behind a gateway; add token auth post-MVP.
- **Concurrent uploads to the same key** → S3 is last-writer-wins per `PUT`; each `PUT` is atomic and becomes its own version, so no corruption, only an interleaving. *Acceptable.*
- **Versioning must precede writes** → *Mitigation:* startup bootstrap (Decision 6).
- **`CopyObject` source-version support** depends on the MinIO server version. *Mitigation:* verified via integration test against the target MinIO in tasks; pin a supported MinIO version.
- **Filename as part of the key** → filenames with `/` could create unintended nested prefixes. *Mitigation:* sanitize/escape the filename or reject filenames containing `/`.

## Migration Plan

Greenfield — no existing data to migrate. Deploy steps:
1. Provision a MinIO instance (or S3-compatible endpoint) and gather credentials.
2. Configure the service (endpoint, access key, secret, bucket name) via env/config.
3. Start the service; bootstrap creates the versioned bucket.
4. Point external systems at the HTTP API.

Rollback strategy (in case of bad deploy): the service is stateless, so redeploy a previous image; data in MinIO is untouched by a service rollback.

## Open Questions

- Maximum expected file size → does the MVP need multipart-upload support, or is single `PUT` streaming enough? (Default: single `PUT` streaming; revisit if files exceed a few hundred MB.)
- Should filenames be validated/rejected for path separators (`/`, `..`), and should the API URL-encode them?
- Do any external systems need to list UUID directories that are still empty? (Current assumption: no.)
