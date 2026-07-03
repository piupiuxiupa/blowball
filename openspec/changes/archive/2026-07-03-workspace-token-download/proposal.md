## Why

当前工作空间文件下载端点 `GET /api/v1/workspace/files/:path` 强制使用 `Authorization: Bearer <jwt>` 请求头鉴权。浏览器原生场景（`<a download>`、`<img src>`、PDF.js 直接 URL、邮件/IM 中分享下载链接）无法自定义请求头，前端只能先 `fetch` 再生成 `blob:` URL，既增加内存占用，也无法直接分享链接。需要提供一个通过 URL query 参数传递 JWT 的下载接口。

## What Changes

- 新增公开 GET 端点 `GET /api/v1/workspace/files/download?token=<jwt>&path=<rel-path>[&inline=1]`：
  - 通过 `token` query 参数完成 JWT 鉴权，不读取 `Authorization` header。
  - 默认返回 `Content-Disposition: attachment`，支持 `inline=1` 预览模式。
  - 文件名使用 RFC 5987 编码（`filename*=utf-8''...`）。
  - 复用现有 `xizhi.ValidatePath` 做路径安全校验。
- 新增 `QueryTokenAuthMiddleware` 中间件，仅对 token-download 路由生效，不影响现有 header-only 接口。
- 前端新增 `getDownloadUrl(path, inline?)` / `getPreviewUrl(path)` 构造器，替换现有 fetch-blob 方案：
  - `ImageViewer` 直接 `<img src={getPreviewUrl(path)}>`。
  - `PdfViewer` 直接 `pdfjs.getDocument({ url: getPreviewUrl(path) })`。
  - `useDownloadFile` 改用 `<a href={getDownloadUrl(path)} download>`。
- 更新 `api/openapi.yaml` 文档。

## Capabilities

### New Capabilities

无新增独立 capability；本次变更属于 workspace 文件下载能力的扩展。

### Modified Capabilities

- `workspace-api`：新增“通过 URL token 下载文件”的需求，扩展 Download file 能力。
- `user-auth`：新增“query token 鉴权”需求，允许特定公开端点通过 `?token=` 参数完成 JWT 验证。

## Impact

- 后端：`internal/middleware/auth.go`、`internal/handler/workspace.go`、`internal/handler/router.go`、`cmd/server/main.go`、`api/openapi.yaml`。
- 前端：`frontend/src/hooks/use-file-content.ts`、`frontend/src/components/files/image-viewer.tsx`、`frontend/src/components/files/pdf-viewer.tsx`、`frontend/src/components/files/binary-placeholder.tsx`。
- 安全：URL 中的 JWT 会进入浏览器历史、Referer、服务端访问日志，需配合 `Referrer-Policy: no-referrer` 与 `Cache-Control: private, no-store` 缓解。
