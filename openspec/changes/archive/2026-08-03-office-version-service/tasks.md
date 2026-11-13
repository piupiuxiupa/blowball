## 1. Project scaffolding

- [x] 1.1 Initialize Go module (`go mod init`) and create the directory layout (e.g., `cmd/server`, `internal/config`, `internal/minio`, `internal/handler`, `internal/router`)
- [x] 1.2 Add dependencies: `github.com/gin-gonic/gin`, `github.com/minio/minio-go/v7`
- [x] 1.3 Add configuration loading (MinIO endpoint, access key, secret, bucket name, listen address) from environment variables

## 2. MinIO client + bootstrap

- [x] 2.1 Create a MinIO client wrapper (construct client from config)
- [x] 2.2 Implement startup bootstrap: check bucket exists; create it if missing; enable Object Versioning; fail fast if versioning cannot be enabled (Requirement: Bucket versioning is ensured at startup)
- [x] 2.3 Add a filename sanitizer/validator that rejects path separators (`/`, `\`, `..`) and URL-encodes filenames for keys

## 3. Storage operations (MinIO-backed)

- [x] 3.1 Implement `Upload(ctx, uuid, filename, reader, size) -> (versionId, lastModified, err)` using `PutObject` (Requirement: Upload a file as a new version)
- [x] 3.2 Implement `ListVersions(ctx, uuid, filename) -> []Version` returning versionId, lastModified, size, isLatest, newest-first (Requirement: List version history of a file)
- [x] 3.3 Implement `DownloadLatest(ctx, uuid, filename) -> (stream, err)` and `DownloadVersion(ctx, uuid, filename, versionId) -> (stream, err)` using `GetObject` (Requirements: Download the latest version; Download a specific historical version)
- [x] 3.4 Implement `ListFiles(ctx, uuid) -> []FileInfo` via `ListObjects` with prefix `{uuid}/` and delimiter `/` (Requirement: List files in a UUID directory)
- [x] 3.5 Implement `ListDirectories(ctx) -> []uuid` via `ListObjects` with delimiter `/` at bucket root (Requirement: List UUID directories)
- [x] 3.6 Implement `Delete(ctx, uuid, filename)` that removes the object and all versions (Requirement: Delete a file and all its versions)

## 4. Rollback operation

- [x] 4.1 Implement `Rollback(ctx, uuid, filename, targetVersionId)` as a server-side `CopyObject` from the target version to the same key; validate the target version exists first; return the new latest versionId (Requirement: Non-destructive rollback to a prior version)

## 5. HTTP layer (Gin)

- [x] 5.1 Define JSON response/error models and a consistent error-to-HTTP-status mapping (404 for not-found, 500 for MinIO errors)
- [x] 5.2 Implement handlers: `POST /documents/:uuid/*filename` (upload), `GET /documents/:uuid/*filename` (download latest), `GET /documents/:uuid/*filename/versions`, `GET /documents/:uuid/*filename/versions/:vid`, `POST /documents/:uuid/*filename/rollback`, `DELETE /documents/:uuid/*filename`
- [x] 5.3 Implement directory handlers: `GET /documents/:uuid` (list files in directory) and `GET /documents` (list UUID directories)
- [x] 5.4 Wire routes in a router and start the Gin server in `cmd/server/main.go` (run bootstrap before serving)

## 6. Testing

- [x] 6.1 Add an integration test harness that starts/points at a MinIO instance (testcontainers or a configurable endpoint) and creates a test bucket
- [x] 6.2 Tests for storage operations: upload creates new version, list history returns versions newest-first with one latest, download latest/specific version matches content, delete removes all versions, cross-UUID filename isolation
- [x] 6.3 Test for rollback: rollback to v1 creates a new latest version equal to v1 content while v1/v2/v3 remain; rollback to unknown version returns not-found
- [x] 6.4 Verify `CopyObject` with a source versionId works against the target MinIO version (Open Question resolution)

## 7. Documentation

- [x] 7.1 Write a README covering setup, configuration env vars, MinIO provisioning, and the HTTP API endpoints with example requests/responses
- [x] 7.2 Document the deferred items (auth, version-retention lifecycle rules, multipart upload) as future work
