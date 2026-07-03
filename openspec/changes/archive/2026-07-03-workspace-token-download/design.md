## Context

当前工作空间文件下载链路：

```
前端 fetch(blob URL)  ←──  GET /api/v1/workspace/files/:path
                              Authorization: Bearer <jwt>
                              AuthMiddleware → WorkspaceHandler.Download
```

`AuthMiddleware` 只从 `Authorization` header 读取 JWT。浏览器原生元素（`<a download>`、`<img>`、PDF.js `getDocument({url})`）无法设置自定义 header，因此前端必须先把文件完整拉进内存生成 `blob:` URL，既浪费内存，也无法生成可分享的永久链接。

## Goals / Non-Goals

**Goals：**
- 新增 `GET /api/v1/workspace/files/download?token=<jwt>&path=<rel-path>` 端点，仅通过 URL query 参数鉴权。
- 默认强制下载（`attachment`），可选 inline 预览（`?inline=1`）。
- 文件名支持 RFC 5987 编码，保证中文文件名正确。
- 复用现有路径校验、错误响应、trace 机制。
- 前端下载/图片/PDF 预览改用直接 URL，不再 fetch blob。

**Non-Goals：**
- 不改现有 header-only 下载端点的行为。
- 不为所有受保护接口开启 URL token 鉴权。
- 不引入短期签名 URL 或独立下载 token（仍使用现有登录 JWT）。
- 不增加文件目录压缩下载、断点续传等新能力。

## Decisions

### 1. 新增独立端点，而不是扩展现有 `AuthMiddleware`

- **选择**：新建 `GET /api/v1/workspace/files/download`，由专属 `QueryTokenAuthMiddleware` 鉴权。
- **理由**：
  - 最小权限原则：只有下载端点接受 URL token，不影响其他接口。
  - 现有 `AuthMiddleware` 职责单一（header Bearer），改动它会让所有受保护接口都接受 URL token，风险面过大。
- **替代方案**：在 `AuthMiddleware` 里 fallback 到 `?token=`。已否决。

### 2. 路径通过 `path` query 参数传递

- **选择**：`?path=reports/2026-q2.md`。
- **理由**：实现简单，与 `GET /workspace/files?path=` 的 listing 接口保持一致，避免额外 catch-all 路由与现有 `*path` 冲突。
- **替代方案**：`/workspace/files/download/*path?token=`。更 RESTful，但会和现有 catch-all 产生更多路由优先级问题；否决。

### 3. 默认 `attachment`，通过 `inline=1` 切换

- **选择**：无 `inline` 参数时强制下载；`inline=1` 时返回 `inline`。
- **理由**：`<a download>` 与直接分享链接的默认行为是下载；图片/PDF 预览显式加 `inline=1`。

### 4. RFC 5987 文件名编码

- **选择**：`Content-Disposition` 同时输出：
  - `filename="ascii-fallback"`（兼容旧浏览器）。
  - `filename*=utf-8''%E4%B8%AD%E6%96%87...`（现代浏览器）。
- **理由**：中文/特殊字符文件名在跨浏览器场景下需要显式编码。

### 5. 复用 `xizhi.ValidatePath` 与 `fs.Store.UserWorkspace`

- **选择**：新 handler 与现有 `Download` 共用同一套路径解析与安全校验。
- **理由**：避免重复实现路径穿越/符号链接逃逸防护，保持安全行为一致。

## Risks / Trade-offs

- **[Risk] JWT 泄露到浏览器历史、Referer、服务端日志** → **Mitigation**：
  - 响应头设置 `Referrer-Policy: no-referrer`。
  - 响应头设置 `Cache-Control: private, no-store`，防止代理/浏览器缓存带 token 的 URL。
  - 文档中声明该端点适用于直接浏览器访问，不建议长期保存链接。
- **[Risk] 路由遮蔽** → `/workspace/files/download` 精确路径会优先匹配；workspace 根下若存在名为 `download` 的文件，将无法通过 header-only catch-all 访问。该情况极罕见，可接受。
- **[Risk] 前端 JWT 暴露在 DOM/HTML 中** → `<img src>`、PDF.js URL 会包含 token，任何能查看页面源码的人都能看到。这与 URL token 方案固有，需在实现注释中说明。

## Migration Plan

- 后端部署后新端点立即可用；现有端点不受影响，无需迁移。
- 前端逐步替换 blob URL：先改 `useDownloadFile`、再改 `ImageViewer`、`PdfViewer`。
- OpenAPI 文档同步更新；前端运行 `npm run generate-api` 重新生成类型（本次新端点不生成复杂类型，类型文件可能无需额外改动）。

## Open Questions

- 是否需要限制 `token` 参数长度？ Gin 默认允许较长 query string，可沿用。
- 生产环境若前端与后端跨域，需确认 CORS 已允许 `GET` 与响应头；当前 `middleware.CORS` 已覆盖。
