## 1. Backend: query-token authentication middleware

- [x] 1.1 Add `QueryTokenAuthMiddleware(secret string) gin.HandlerFunc` in `internal/middleware/auth.go`
- [x] 1.2 Read `token` from `c.Query("token")`; return 401 `missing token` if empty
- [x] 1.3 Call `jwt.Verify` and map errors to `invalid token` / `token expired`
- [x] 1.4 Set `user_id` on gin.Context and call `c.Next()` on success
- [x] 1.5 Add unit tests in `internal/middleware/auth_test.go` covering valid/missing/invalid/expired query token

## 2. Backend: token-download handler

- [x] 2.1 Add `TokenDownload` method to `WorkspaceHandler` in `internal/handler/workspace.go`
- [x] 2.2 Read `path` query parameter; return 400 if missing/empty
- [x] 2.3 Resolve workspace root via `fsSvc.UserWorkspace(userID)` and validate with `xizhi.ValidatePath`
- [x] 2.4 Return 403 for path outside workspace, 404 for missing file, 400 for directory
- [x] 2.5 Add `contentDisposition(name string, inline bool) string` helper with RFC 5987 encoding
- [x] 2.6 Set `Content-Disposition`, `Cache-Control: private, no-store`, `Referrer-Policy: no-referrer`
- [x] 2.7 Serve file with `c.File(abs)`
- [x] 2.8 Add unit tests in `internal/handler/workspace_test.go`

## 3. Backend: routing and wiring

- [x] 3.1 Add `WorkspaceTokenDownload gin.HandlerFunc` and `QueryTokenAuthMW gin.HandlerFunc` to `RouteDeps` in `internal/handler/router.go`
- [x] 3.2 Register `GET /workspace/files/download` on `v1` group (outside `authed`) with `QueryTokenAuthMW` then `WorkspaceTokenDownload`
- [x] 3.3 Wire middleware and handler in `cmd/server/main.go` using `cfg.JWT.Secret`
- [x] 3.4 Verify gin route priority: exact `/workspace/files/download` matches before catch-all `*path`

**Note on 3.2/3.4:** gin rejects a static `/workspace/files/download` route as a sibling of `/workspace/files/*path` (it forbids a static segment and a wildcard at the same tree node regardless of registration order). The endpoint is therefore served through the existing `/*path` catch-all with a route-level auth selector: path == `"download"` uses `QueryTokenAuthMW`, all other paths continue to use `AuthMW`.

## 4. Backend: API documentation

- [x] 4.1 Add `GET /api/v1/workspace/files/download` path to `api/openapi.yaml`
- [x] 4.2 Document query parameters `token`, `path`, `inline`
- [x] 4.3 Document responses 200 (file bytes), 400, 401, 403, 404, 500
- [x] 4.4 Set `security: []` for this operation (auth is explicit in query)

## 5. Frontend: URL helpers

- [x] 5.1 Add `getDownloadUrl(path: string): string` in `frontend/src/hooks/use-file-content.ts`
- [x] 5.2 Add `getPreviewUrl(path: string): string` (same endpoint with `inline=1`)
- [x] 5.3 Reuse `getApiBase()` and `getToken()`; properly URL-encode path and token

## 6. Frontend: replace blob-based viewers

- [x] 6.1 Update `ImageViewer` to use `<img src={getPreviewUrl(path)}>` and remove `useImageBlob`
- [x] 6.2 Update `PdfViewer` to pass `getPreviewUrl(path)` to `pdfjs.getDocument({ url })` and remove `useDownloadUrl`
- [x] 6.3 Update `useDownloadFile` to create `<a href={getDownloadUrl(path)} download>` and click it
- [x] 6.4 Clean up unused blob/object URL revocation code

**Note:** `ExcelViewer` and `WordViewer` also depended on `useDownloadUrl`; they were updated to fetch from `getPreviewUrl(path)` directly so the hook could be removed entirely.

## 7. Frontend: cleanup and types

- [x] 7.1 Remove now-unused TanStack Query keys `image-blob` and `download-url` from `useDeleteFile` sync logic
- [x] 7.2 Run `npm run generate-api` in `frontend/` to refresh types from OpenAPI
- [x] 7.3 Run `npm run lint` (type-check) and fix errors

## 8. Verification

- [x] 8.1 Run `go test ./internal/middleware/... ./internal/handler/...`
- [x] 8.2 Run `go test ./test/integration/...`
- [x] 8.3 Run `make lint`
- [x] 8.4 Manual end-to-end: login, upload a file with Chinese filename, verify direct URL downloads and previews
