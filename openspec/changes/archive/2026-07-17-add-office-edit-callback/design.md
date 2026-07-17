## Context

办公文件已能通过本地 OnlyOffice DocumentServer 只读渲染，但现有实现把 secret、服务地址**硬编码在前端**（`src/lib/onlyoffice.ts` 的 `ONLYOFFICE` 常量），并在浏览器侧用 Web Crypto 自签 config——secret 外泄面大、不可配置。同时编辑无法落盘：OnlyOffice 把改动暂存在 DocumentServer 容器内，触发保存时由 **DocumentServer 服务端** POST 一个 JSON 回调到 `callbackUrl`（改完的文件在 body 的 `url` 字段，**不在 body 里**），而 blowball 没有这个回调端点。

本变更把所有 OnlyOffice 参数收敛到后端 `config.yaml`，由后端签发编辑器配置（secret 不出后端），并新增保存回调端点。

现有后端集成点：
- `internal/handler/workspace.go`：`WorkspaceHandler` 已有 `Upload`/`Download`/`TokenDownload`/`Content`/`Delete`；写盘与路径校验（`xizhi.ValidatePath`）可复用。
- `internal/handler/router.go`：`RouteDeps` + `RegisterRoutes`；工作区 GET 走单 catch-all + 后缀/前缀分发（`/content` 后缀、`download/` 前缀）；`QueryTokenAuthMiddleware`（`internal/middleware/auth.go:54`）从 query 读 JWT；`AuthMiddleware` 校验 Bearer。
- `internal/pkg/jwt`：HS256 签名/校验（复用 `golang-jwt/v5`）。
- `internal/config/config.go`：YAML 加载 + `${VAR}` 展开 + 默认值。

## Goals / Non-Goals

**Goals:**
- 办公文件可编辑并持久化回工作区。
- OnlyOffice secret、服务地址全部来自 `config.yaml`；secret 只存在后端，绝不下发浏览器。
- 复用现有鉴权与路径校验；回调 `url` 下载做 host 白名单，避免 SSRF。
- 编辑保存后再次打开看到最新内容。

**Non-Goals:**
- 多人实时协同的产品化。
- WOPI 协议、版本历史、冲突合并、文件锁。
- 回调自带的 OnlyOffice outbox JWT 校验（见 Risks，prod 可加）。

## Decisions

### 决策 1：配置驱动，后端签发配置（secret 不出后端）
**选择**：`config.yaml` 新增 `onlyoffice: { secret, server_url, internal_backend }`。新增 `GET /api/v1/workspace/files/*path/onlyoffice-config`（Bearer 鉴权），后端读取 secret，构造 config（含 `document.url`/`callbackUrl`/随机 key/edit 模式），用 secret 做 HS256 签名，返回 `{ server_url, config, token }`。前端只负责"取配置 → 用 server_url 加载 api.js → `new DocEditor(id, {...config, token})`"，**不持有 secret、不自签**。
**理由**：secret 是服务端密钥，进浏览器 bundle 即外泄；后端签发是 OnlyOffice 官方推荐的安全姿势。同时消除前端三类硬编码（secret/apiScript/internalBackend）。
**备选**：继续前端硬编码 + 浏览器自签（现状）——被否决，不可配置且不安全。

### 决策 2：两个端点的路由形态
**选择**：
- 配置端点 `GET /api/v1/workspace/files/*path/onlyoffice-config`——并入既有 catch-all，加 `/onlyoffice-config` 后缀分发，Bearer 鉴权（与 `/content` 同模式）。
- 回调端点 `POST /api/v1/workspace/onlyoffice-callback?path=&token=`——独立 POST 路由，挂 `QueryTokenAuthMiddleware`（与 token-download 同模式）。
**理由**：配置是"按文件 GET、走 Bearer"，天然契合 catch-all 后缀模式；回调是"容器 POST、走 query-token"，独立路由最干净、避开 gin 通配冲突。两者都复用现有鉴权模式，零新中间件。

### 决策 3：文档 key 随机生成
**选择**：后端每次签发配置时生成随机 key（如 `crypto/rand` 16 字节 → base32）。每次打开 = 新 key = OnlyOffice 重新拉取转换。
**理由**：彻底规避"保存后 OnlyOffice 吐旧缓存"；前端无需透传文件 mtime、保存后无需失效文件列表。代价是每次打开重新转换（~2-3s）。
**备选**：key 含 size+update_time（文件列表已返回，转换更快）——被否决：要前端按 `activeFilePath` 去文件树缓存取元数据、保存后还要失效列表，复杂度高；随机 key 更简单且永远新鲜。

### 决策 4：编辑模式 + forcesave
**选择**：config 用 `mode:"edit"`、`permissions.edit:true`、`customization.forcesave:true`；回调处理 `status=6`（forcesave）与 `status=2`（关闭保存）。
**理由**：默认仅关闭时保存体验差；forcesave 让定时/手动保存都落盘。

### 决策 5：document.url / callbackUrl 用 internal_backend + 用户 JWT
**选择**：后端拼 `document.url = internal_backend + /api/v1/workspace/files/download/<path>?inline=1&token=<userJWT>`、`callbackUrl = internal_backend + /api/v1/workspace/onlyoffice-callback?path=<enc>&token=<userJWT>`。`userJWT` 取自配置请求的 Bearer token（透传，复用其剩余有效期）。
**理由**：`internal_backend` 是容器→后端可达地址（如 `http://10.1.152.201:8080`），与现有只读渲染同源约束；复用现有 query-token 下载与回调鉴权，不造新凭据。
**代价**：用户 JWT（默认 7d）嵌入 URL，超长会话保存可能 401（见 Risks）。

### 决策 6：结果 url host 白名单（缓解 SSRF）
**选择**：回调 handler 用标准库 `http.Client`（带超时）GET 回调 `url`；**仅当 `url` 的 host 等于 `onlyoffice.server_url` 的 host 时才下载**，否则拒绝（`{"error":1}`）。
**理由**：`url` 来自 body，放任任意 host 是 SSRF + 任意写。白名单收窄到 DocumentServer。`server_url` 兼作浏览器加载 api.js 的地址与白名单基准，保证二者一致。

### 决策 7：原子写回
**选择**：下载到目标同目录临时文件，成功后 `os.Rename` 覆盖；任一步失败返回 `{"error":1}`、原文件不动；字节数超 `maxUploadBytes` 拒绝。
**理由**：避免下载/写盘中断损坏原文件；与 agent 并发写更安全。

## Risks / Trade-offs

- **[用户 JWT 嵌入 URL 过期]** `document.url`/`callbackUrl` 里的用户 JWT 默认 7d，超长编辑会话保存可能 401。→ 开发可接受；prod 可改为签发长效 workspace 专用 token，或校验 OnlyOffice outbox JWT（决策见下）。
- **[回调鉴权=用户 JWT]** 持有用户 JWT 者可伪造回调覆盖该用户文件。→ 已被 host 白名单（决策 6）收窄 url 面；失窃用户 JWT 本身即高危；prod 可加 OnlyOffice outbox JWT 校验（需后端持 secret 做通用验签，现 `jwt.Verify` 只认 `user_id` claim，需扩展）。
- **[随机 key 每次重转]** 每次打开重新转换 ~2-3s。→ 用户已接受（显式选择"随机兜底"）；后续若嫌慢可切 size+update_time 方案。
- **[结果 url host 漂移]** DocumentServer siteUrl 若与 `server_url` 的 host 不一致，合法回调会被拒。→ 部署时确保一致；handler 日志打印被拒 url 便于排查。
- **[并发写]** agent 与用户同写一文件可能互相覆盖。→ 原子 rename 缩小窗口；不加锁（non-goal）。

## Migration Plan

- 纯增量：新增配置段（带默认值，老 config 不破坏）、两个端点、前端改调用方式。
- 部署：在 `config.yaml` 填 `onlyoffice.secret`（与 DocumentServer `local.json` 一致）、`server_url`、`internal_backend`。
- 回滚：移除路由注册、前端切回只读（或保留只读渲染路径）。
- `onlyoffice` 段缺失时：编辑相关端点返回 503/禁用，只读渲染也需相应配置项（迁移现有硬编码到配置）。

## Open Questions

- 是否需要限制回调可写文件最大字节数？建议复用 `maxUploadBytes`（已在决策 7）。
- 前端是否保留"刷新"按钮触发重新取配置（拿新随机 key 强制重转）？建议保留。
