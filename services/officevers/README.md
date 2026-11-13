# office-version-service

> **Provenance**: this service was incorporated into the blowball monorepo
> (change `monorepo-restructure`) from the standalone `office-vers` repository,
> last upstream commit `99ef6c2` ("Address files by logical path via SHA-256
> hash keys"). Import paths were rewritten to
> `github.com/lush/blowball/services/officevers/...`; runtime behavior, config,
> and the default `:8088` listener are unchanged.

A small, stateless HTTP service that provides **versioned file storage** for
Office (and arbitrary) documents on top of [MinIO][minio] (or any S3-compatible
object store). It uses MinIO's **native S3 Object Versioning** as the sole
mechanism for retaining history — every upload to the same file becomes a new
version, any version can be downloaded, and a non-destructive rollback promotes a
prior version's content to the new latest version without discarding any history.

Files are namespaced under a caller-supplied **directory UUID**. Clients address
files by a **logical path** (which may include `/`-separated directories); the
service hashes the path into a flat storage key `{uuid}/{<sha256>}{<ext>}`, so
each external system's files are isolated and independently versioned.

- **No database** — MinIO is the single source of truth.
- **No authentication** in the MVP — see [Future work](#future-work).
- Written in Go with [Gin][gin] and [minio-go v7][minio-go].

---

## Table of contents

- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [MinIO provisioning](#minio-provisioning)
- [HTTP API](#http-api)
  - [Upload a new version](#upload-a-new-version)
  - [Download the latest version](#download-the-latest-version)
  - [Download a specific version](#download-a-specific-version)
  - [List version history](#list-version-history)
  - [Rollback to a prior version](#rollback-to-a-prior-version)
  - [Delete a file and all versions](#delete-a-file-and-all-versions)
  - [List UUID directories](#list-uuid-directories)
  - [Health check](#health-check)
- [Error responses](#error-responses)
- [Testing](#testing)
- [Project layout](#project-layout)
- [Future work](#future-work)

---

## How it works

- A single **versioned** MinIO bucket holds every file.
- Clients address each file by a **logical path** (it may contain `/`-separated
  directory segments). The service derives the storage key by hashing the full
  logical path with SHA-256 and storing the object at
  `{uuid}/{<sha256-hex>}{<ext>}`, so directory structure is flattened and the
  `{uuid}` namespace isolates files across external systems. Because the hash is
  deterministic on the path (not the content), every upload to the same logical
  path becomes a new version of the same object.
- On startup the service **bootstraps** the bucket: it creates the bucket if it
  does not exist and enables Object Versioning if it is not already on. If
  versioning cannot be enabled, the service **fails fast** and refuses to serve
  requests — versions written before versioning is on cannot be recovered
  retroactively.
- Every upload (`PUT`) to a key produces a **new version** with a fresh
  `versionId`; prior versions are retained automatically by MinIO.
- **Rollback** is implemented as a server-side `CopyObject` from a target version
  to the same key. This creates a *new* latest version whose content equals the
  target, and **keeps every existing version** (including those newer than the
  target). Rollback is therefore always undoable.

## Quick start

Requirements: Go 1.26+ and a reachable MinIO instance.

```bash
# 1. Start MinIO (example via Docker)
docker run -d --name minio -p 9000:9000 \
  -e MINIO_ROOT_USER=minioadmin \
  -e MINIO_ROOT_PASSWORD=minioadmin \
  minio/minio server /data

# 2. Build and run the service
go build -o office-version-service ./cmd/server
export MINIO_ENDPOINT=127.0.0.1:9000
export MINIO_ACCESS_KEY=minioadmin
export MINIO_SECRET_KEY=minioadmin
export MINIO_BUCKET=office-documents   # created automatically with versioning
export LISTEN_ADDR=:8080
./office-version-service

# 3. Upload two versions of a file under UUID "abc"
curl -s -X POST --data-binary "v1-content" \
  http://localhost:8080/documents/abc/report.docx
curl -s -X POST --data-binary "v2-content" \
  http://localhost:8080/documents/abc/report.docx
```

## Configuration

All configuration is via environment variables:

| Variable           | Required | Default  | Description                                            |
| ------------------ | :------: | :------: | ------------------------------------------------------ |
| `MINIO_ENDPOINT`   |   yes    |          | MinIO host:port, e.g. `127.0.0.1:9000`                 |
| `MINIO_ACCESS_KEY` |   yes    |          | MinIO access key                                       |
| `MINIO_SECRET_KEY` |   yes    |          | MinIO secret key                                       |
| `MINIO_BUCKET`     |   yes    |          | Bucket used to store files (created if missing)        |
| `MINIO_USE_SSL`    |    no    | `false`  | `true` to use TLS for the MinIO connection             |
| `MINIO_REGION`     |    no    |          | Optional region for the MinIO endpoint                 |
| `LISTEN_ADDR`      |    no    | `:8080`  | HTTP address the service listens on                    |

### Command-line flags

Every setting can also be supplied via command-line flags, each mapping 1:1 to
an environment variable (flag names are the kebab-case form of the env var).

| Flag                 | Env var           | Default | Description                                            |
| -------------------- | ----------------- | :-----: | ------------------------------------------------------ |
| `--minio-endpoint`   | `MINIO_ENDPOINT`   |         | MinIO host:port                                        |
| `--minio-access-key` | `MINIO_ACCESS_KEY` |         | MinIO access key                                       |
| `--minio-secret-key` | `MINIO_SECRET_KEY` |         | MinIO secret key                                       |
| `--minio-bucket`     | `MINIO_BUCKET`     |         | Bucket used to store files (created if missing)        |
| `--minio-use-ssl`    | `MINIO_USE_SSL`    | `false` | Use TLS for the MinIO connection                       |
| `--minio-region`     | `MINIO_REGION`     |         | Optional region for the MinIO endpoint                 |
| `--listen-addr`      | `LISTEN_ADDR`      | `:8080` | HTTP address the service listens on                    |

**Precedence** (highest to lowest): **command-line flags > environment variables > built-in defaults**. A flag overrides the corresponding env var; an unset flag falls back to the env var; if neither is set the built-in default is used.

Flags accept a single dash (`-minio-bucket b`) or double dash (`--minio-bucket b`). Use `-h` / `--help` to print usage.

```bash
# Configure entirely via flags (no env vars needed)
./office-version-service \
  --minio-endpoint 127.0.0.1:9000 \
  --minio-access-key minioadmin \
  --minio-secret-key minioadmin \
  --minio-bucket office-documents \
  --listen-addr :8080

# Override a single value from the environment with a flag
MINIO_BUCKET=from-env ./office-version-service --minio-bucket override-bucket
# -> uses bucket "override-bucket"
```

## MinIO provisioning

The service creates and version-enables the bucket at startup, so usually you
only need credentials and a bucket name. To provision MinIO manually:

```bash
docker run -d --name minio -p 9000:9000 \
  -e MINIO_ROOT_USER=minioadmin \
  -e MINIO_ROOT_PASSWORD=minioadmin \
  minio/minio server /data
```

The service needs permission to create the bucket (if missing), and to get/set
bucket versioning and to read/write/list/delete objects in it.

## HTTP API

Base path: `/documents`. File operations live under `/documents/:uuid/*filepath`,
where `*filepath` is the file's **logical path** — a catch-all that may itself
contain `/`-separated directory segments (e.g. `财务/2024/年报.docx`). The
operation is selected by the HTTP method and an explicit `?action=` query
parameter, so logical paths may freely include segments named `versions` or
`rollback`.

| Method   | Path                                                        | Operation                          |
| -------- | ---------------------------------------------------------- | ---------------------------------- |
| `POST`   | `/documents/:uuid/*filepath`                               | Upload a new version               |
| `GET`    | `/documents/:uuid/*filepath`                               | Download the latest version        |
| `GET`    | `/documents/:uuid/*filepath?action=versions`               | List version history               |
| `GET`    | `/documents/:uuid/*filepath?action=version&versionId=VID` | Download a specific version        |
| `POST`   | `/documents/:uuid/*filepath?action=rollback`               | Rollback to a prior version        |
| `DELETE` | `/documents/:uuid/*filepath`                               | Delete the file and all versions   |
| `GET`    | `/documents`                                               | List UUID directories              |

The logical path is hashed (SHA-256 over the full path including its extension)
into the flat storage key `{uuid}/{<sha256-hex>}{<ext>}`; the path itself is not
stored verbatim. Logical paths may not be empty, may not contain empty segments
(`a//b`), `.` / `..` segments, backslashes, or NUL/control characters (HTTP 400).
No Unicode normalization or case-folding is applied — the exact bytes sent are
hashed, so a client must address a file with a byte-identical path on upload and
download.

### Upload a new version

`POST /documents/:uuid/*filepath`

The request body is the raw file bytes. The request's `Content-Type` is ignored
for storage; the stored object's content type is derived from the logical path's
extension (Office OOXML types included). A chunked body (no `Content-Length`) is
streamed via multipart upload.

**Request**

```bash
# A flat logical path:
curl -X POST --data-binary @report.docx \
  http://localhost:8080/documents/abc/report.docx

# A logical path with directories:
curl -X POST --data-binary @report.docx \
  http://localhost:8080/documents/abc/财务/2024/年报.docx
```

**Response** `201 Created`

```json
{
  "versionId": "7a604dc7-719f-426f-88c4-642313fd2860",
  "lastModified": "2026-08-02T06:42:15Z"
}
```

### Download the latest version

`GET /documents/:uuid/*filepath`

Streams the content of the current latest version. The response includes
`Content-Type`, `Content-Length`, `Last-Modified`, and an `X-Version-Id` header.

**Response** `200 OK` with the file bytes. `404` if the file does not exist.

### Download a specific version

`GET /documents/:uuid/*filepath?action=version&versionId=<vid>`

Streams the exact content of the requested version, even if newer versions exist.
`404` if the file or version id does not exist.

### List version history

`GET /documents/:uuid/*filepath?action=versions`

Returns the version history ordered newest-first. Exactly one entry is marked
`isLatest`.

**Response** `200 OK`

```json
{
  "uuid": "abc",
  "path": "report.docx",
  "versions": [
    {
      "versionId": "063807c3-09cb-449c-b889-6872c77c7a76",
      "lastModified": "2026-08-02T06:41:08.568Z",
      "size": 10,
      "isLatest": true
    },
    {
      "versionId": "7a099e4a-c79b-4d3b-a93f-b09394b43809",
      "lastModified": "2026-08-02T06:41:08.552Z",
      "size": 10,
      "isLatest": false
    }
  ]
}
```

`404` if the file has never been uploaded.

### Rollback to a prior version

`POST /documents/:uuid/*filepath?action=rollback`

Non-destructively promotes the target version's content to become the new latest
version (via a server-side copy). **All existing versions remain**, including
those newer than the target.

The target version id is supplied as a JSON body, or as a `?versionId=` query
parameter.

**Request**

```bash
curl -X POST -H 'Content-Type: application/json' \
  -d '{"versionId":"7a604dc7-719f-426f-88c4-642313fd2860"}' \
  'http://localhost:8080/documents/abc/report.docx?action=rollback'
```

**Response** `200 OK`

```json
{
  "versionId": "755f7c99-bcd8-4eb7-9b31-300ade4b0280"
}
```

`400` if no `versionId` is provided; `404` if the file or the target version does
not exist.

### Delete a file and all versions

`DELETE /documents/:uuid/*filepath`

Removes the object together with **every** historical version (and any delete
markers). Idempotent — deleting a non-existent object succeeds.

**Response** `204 No Content`.

### List UUID directories

`GET /documents`

Returns the top-level UUID directories that contain at least one file.

**Response** `200 OK`

```json
{
  "directories": ["abc", "xyz"]
}
```

### Health check

`GET /healthz` → `200 OK`

```json
{ "status": "ok" }
```

## Error responses

All errors use a consistent JSON body:

```json
{ "error": { "message": "not found" } }
```

Status mapping:

| Status | Meaning                                                |
| ------ | ------------------------------------------------------ |
| `400`  | Invalid input (bad logical path, missing version id, ...)  |
| `404`  | File or version not found                              |
| `500`  | Unexpected / MinIO error                               |

## Testing

### Unit tests (no external dependencies)

```bash
go test ./...
```

### Integration tests (require a live MinIO)

The integration tests verify the storage layer, rollback, and versioned
`CopyObject` against a real MinIO. They are skipped unless the `TEST_MINIO_*`
environment variables are set.

```bash
# Start MinIO
docker run -d --name minio -p 9000:9000 \
  -e MINIO_ROOT_USER=minioadmin \
  -e MINIO_ROOT_PASSWORD=minioadmin \
  minio/minio server /data

# Run integration tests
TEST_MINIO_ENDPOINT=127.0.0.1:9000 \
TEST_MINIO_ACCESS_KEY=minioadmin \
TEST_MINIO_SECRET_KEY=minioadmin \
go test ./internal/minio/ -run Integration -v
```

The harness creates a fresh versioned bucket per run.

## Project layout

```
cmd/server/         # HTTP server entrypoint (config -> bootstrap -> serve)
internal/
  config/           # environment-based configuration
  minio/            # MinIO client, bootstrap, sanitizer, storage ops, rollback
  handler/          # Gin HTTP handlers + JSON models + error mapping
  router/           # route wiring
```

## Future work

The following are intentionally **out of scope** for the MVP and tracked for
later:

- **Authentication / authorization.** The service is open by design; deploy it
  on a private network or behind a gateway, and add token auth post-MVP.
- **Version retention / lifecycle rules.** Versions accumulate indefinitely. Add
  an S3 lifecycle rule for non-current version expiration (e.g. keep last N
  versions or versions younger than N days) — no code change required.
- **Multipart upload tuning.** Single-`PUT` streaming is used by default; revisit
  part size / concurrency thresholds if very large files (> a few hundred MB)
  become common.

[minio]: https://min.io/
[minio-go]: https://github.com/minio/minio-go
[gin]: https://github.com/gin-gonic/gin
