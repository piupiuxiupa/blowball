## Why

办公文件（docx/xlsx/pptx 等）目前能通过 OnlyOffice DocumentServer **只读渲染**，但编辑无法落盘——OnlyOffice 把改完的文件只发到一个 `callbackUrl`，而 blowball 后端没有这个回调接口，所以编辑内容在关闭文档后就丢失。同时现有只读实现把 secret、服务地址等**硬编码在前端**，既不安全（secret 进浏览器 bundle）也不可配置。本变更要让办公文件真正可编辑、可保存，并把所有 OnlyOffice 相关参数收敛到后端配置。

## What Changes

- **新增"编辑器配置签名"接口** `GET /api/v1/workspace/files/*path/onlyoffice-config`：后端用配置中的 secret 对编辑器 config 做 HS256 签名，返回 `{ server_url, config, token }`。config 内含编辑模式、随机 `key`、`document.url`、`callbackUrl` 等。**secret 只存在后端，永不下发浏览器。**
- **新增"保存回调"接口** `POST /api/v1/workspace/onlyoffice-callback`：接收 OnlyOffice 的 JSON 回调（`status`/`key`/`url`），在保存状态（2/6）下载 `url` 并**原子写回** `data/{userID}/workspace/{path}`，返回 `{"error":0}`；其余状态仅确认。
- **配置驱动**：`config.yaml` 新增 `onlyoffice: { secret, server_url, internal_backend }`——secret（签名）、server_url（浏览器加载 api.js 的 DocumentServer 地址，兼作结果 url 的 host 白名单）、internal_backend（容器→后端可达地址，用于拼 `document.url`/`callbackUrl`）。**移除前端一切硬编码**。
- **回调鉴权复用 query-token**：后端把用户 JWT 拼进 `callbackUrl` 的 query，复用 `QueryTokenAuthMiddleware`。
- **文档 key 用随机值**：后端每次签发配置时生成随机 key。每次打开 = 新 key = OnlyOffice 重新拉取，根治"保存后看到旧缓存"；代价是每次打开重新转换（~2-3s，可接受）。无需前端透传文件 mtime、无需保存后失效文件列表。
- **编辑模式 + forcesave**：config 用 `mode: edit`、`permissions.edit: true`、`customization.forcesave: true`。
- 结果 `url` 下载做 host 白名单（仅 `server_url` 的 host），缓解 SSRF。

## Capabilities

### New Capabilities
- `office-file-editing`: 通过 OnlyOffice 编辑办公文件并持久化回工作区的能力——包含"签发已签名编辑器配置"与"接收保存回调落盘"两个端点的契约（鉴权、配置签名、随机 key、状态处理、原子写回、url host 白名单）。

### Modified Capabilities
<!-- 无既有 spec 的需求层变更。两个端点均为新增，独立成 capability，不改 workspace-api 的既有需求。 -->

## Impact

- **后端**：`internal/config/config.go` 增 `onlyoffice` 配置段；`internal/handler/workspace.go` 新增 `OnlyOfficeConfig`（签发）与 `OnlyOfficeCallback`（落盘）两个 handler；`internal/handler/router.go`（`RouteDeps` + `RegisterRoutes`）注册一个 GET（Bearer 鉴权）+ 一个 POST（query-token 鉴权）；`cmd/blowball/serve.go` 接线；复用 `xizhi.ValidatePath`、`internal/pkg/jwt` 的 HS256。
- **前端**：`src/lib/onlyoffice.ts` 重构——**删除硬编码常量与浏览器侧签名**，改为调用后端配置接口拿 `{server_url, config, token}`；`src/components/files/office-viewer.tsx` 改为"取配置 → 加载 api.js → 实例化"；删除 `public/oo-test.html`（调试用）。
- **API 文档**：`api/openapi.yaml` 增加两个端点。
- **配置**：`config.yaml` / `config.example.yaml` 新增 `onlyoffice` 段（含 secret）；`.env`/部署文档说明 secret 保密。
- **依赖**：无新增（HS256 复用 `golang-jwt/v5`，HTTP 下载用标准库）。
- **安全**：secret 从前端 bundle 移到后端配置，消除"secret 外泄"面；回调 url host 白名单消除 SSRF 任意写。
