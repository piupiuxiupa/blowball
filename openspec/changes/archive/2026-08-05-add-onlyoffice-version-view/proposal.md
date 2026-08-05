## Why

历史版本预览需要一个专用的 OnlyOffice 配置端点。现有 `GET /api/v1/workspace/files/*path/onlyoffice-config` 只面向**当前活动文件**：`document.url` 指向 blowball 自己的下载端点（取工作区里的现文件），`document.key` 每次请求随机生成。它无法把 `document.url` 指向**外部 office-vers 服务**里某个**特定历史版本**的字节，也就无法在编辑器内只读浏览某一版历史内容。

office-vers（独立服务，MinIO 原生版本化、MVP 无鉴权）已能以 `GET /documents/{uuid}/{path}?action=version&versionId=<vid>` 流式返回某一历史版本。本变更新增一个**只读、版本钉定**的配置端点，让前端传 `versionId`、后端签发一份 `document.url` 指向 office-vers 该版本、`mode:"view"` 的 DocEditor 配置。

## What Changes

- **新增端点** `GET /api/v1/workspace/files/*path/onlyoffice-version-config?versionId=<vid>`：与既有 `/onlyoffice-config` 同属工作区 GET catch-all 的**后缀分发**（新增后缀 `/onlyoffice-version-config`），Bearer JWT 鉴权，注册在 API 分区。
- **响应结构仿照现有格式**（嵌套对称、复用前端 `resp.view.*` 消费路径），但**只返回 view 一种模式**（历史版本不可变，无 edit 语义）：

  ```json
  { "server_url": "<string>", "view": { "config": <object>, "token": <string> } }
  ```

- **`document.url` 指向 office-vers**：`{onlyoffice.version_service_url}/documents/{userUUID}/{path}?action=version&versionId=<vid>`。office-vers 是文件来源，且按设计无鉴权，故 **不在 url 上嵌入用户 JWT**（与既有端点把 JWT 嵌入 `document.url` 不同）。
- **`document.key` 按版本确定性派生**：`base32(sha256(path + ":" + versionId))`（小写、无 padding，与既有 `randomOnlyOfficeKey` 编码风格一致）。这是对既有端点"每次随机 key"策略的**有意背离**——历史版本不可变（MinIO 版本化只能追加/回滚产生新 latest，不能原地改写某一版），确定性 key 让 OnlyOffice 缓存并跨打开/跨用户复用同一版本的转换结果；不同版本自然得到不同 key。
- **view-only 内容约束**：`editorConfig.mode:"view"`、`document.permissions:{ edit:false, download:true }`；**不含** `callbackUrl`（历史版本无保存语义、无落盘目标）、**不含** `customization.forcesave`。
- **新增配置字段** `onlyoffice.version_service_url`（office-vers 服务基址）。该端点要求 `onlyoffice.secret` **与** `version_service_url` 同时非空，否则返回 503（复用既有"未配置即 503"的闸门模式）。
- **`versionId` 必填**：缺失返回 400。
- **复用**：`configured()` 闸门、`xizhi.ValidatePathAllowReserved` 越界校验、`onlyOfficeDocumentType`、`jwt.SignClaims`/`signOnlyOfficeConfig`、`onlyOfficeConfigResponse` 的 `{config, token}` 子结构与 `onlyOfficeModeConfig` 类型。
- **非目标**：不涉及列版本历史、回滚、上传版本、代理 office-vers，也不对 `versionId` 是否真实存在于 office-vers 做主动校验（**懒校验**——版本不存在时由 DocumentServer 在拉取 `document.url` 时 404）。blowball 在本端点中**仅作为配置签发方**，office-vers 数据面由别处掌控。

## Capabilities

### New Capabilities
<!-- 无新增 capability。 -->

### Modified Capabilities
- `office-file-editing`：新增需求"签发已签名的 OnlyOffice 历史版本只读配置"（新端点 `.../onlyoffice-version-config?versionId=`）。既有的"签发已签名的 OnlyOffice 编辑器配置"需求（面向活动文件的双模式 `.../onlyoffice-config`）**完全不变**。

## Impact

- **后端**
  - `internal/handler/workspace.go`：新增 `OnlyOfficeVersionConfig` handler 与 `buildOnlyOfficeVersionConfig(rel, versionID, userUUID)` 构造函数（确定性 key + office-vers `document.url` + view-only config）；新增响应类型或复用现有 `onlyOfficeConfigResponse`/`onlyOfficeModeConfig`。
  - `internal/handler/router.go`：新增 `onlyOfficeVersionConfigSuffix = "/onlyoffice-version-config"` 常量、`RouteDeps.WorkspaceOnlyOfficeVersionConfig` 字段、`dispatchWorkspaceFile` 内第三个后缀分支。
  - `internal/config/config.go`：`OnlyOfficeConfig` 新增 `VersionServiceURL string` 字段（yaml `version_service_url`，支持 `${VAR}` 展开）。
  - `cmd/blowball/serve.go`：构造 `WorkspaceHandler` 时传入新字段；`RouteDeps.WorkspaceOnlyOfficeVersionConfig = wsH.OnlyOfficeVersionConfig`。
- **API 文档**：`api/openapi.yaml` 新增端点与响应 schema；复制到 blowball-frontend 并 `npm run generate-api`。
- **测试**：`internal/handler/workspace_test.go` 新增该端点用例（200 正常签发、key 确定性、url 指向 office-vers、view 内容约束、versionId 缺失 400、路径越界 403、未鉴权 401、未配置 503）；既有 `onlyoffice-config` 与回调用例不回归。
- **前端**（blowball-frontend 独立仓）：消费 `resp.view.{config, token}` 实例化 DocEditor，请求时带 `versionId`。
- **配置/依赖**：无新依赖；复用 `internal/pkg/jwt` 与既有 OnlyOffice 基建。新增一个配置字段。
- **安全**：secret 仍只在后端；`document.url` 不含凭据（依赖 office-vers 私网部署 + 能力型 URL，其中 `versionId` 为 128-bit MinIO UUID 充当不可猜测的能力凭据）；本端点无落盘、无回调，SSRF/原子落盘逻辑不受影响。
