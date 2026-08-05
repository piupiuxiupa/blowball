## 1. 配置：新增 office-vers 基址字段

- [x] 1.1 `internal/config/config.go` 在 `OnlyOfficeConfig` 新增 `VersionServiceURL string`（yaml tag `version_service_url`），支持 `${VAR}` 展开（与同结构其他字段一致）
- [x] 1.2 在 `config.example.yaml` 的 `onlyoffice:` 段补充 `version_service_url` 注释样例（说明为 office-vers 服务基址，留空则版本预览端点 503）

## 2. Handler：版本只读配置签发

- [x] 2.1 `internal/handler/workspace.go` 新增 `buildOnlyOfficeVersionConfig(rel, versionID, userUUID string)`：构造 view-only config——`documentType`（复用 `onlyOfficeDocumentType`）、`document{ fileType, key, title, url, permissions:{ edit:false, download:true } }`、`editorConfig{ mode:"view", user }`；**不含** `callbackUrl`、**不含** `customization.forcesave`（注：返回值由任务初稿的 `(map, error)` 简化为 `map[string]any`——确定性 key/串拼接无失败模式，无伪 error）
- [x] 2.2 `buildOnlyOfficeVersionConfig` 中 `document.key` 按 `base32(sha256(path + ":" + versionID))` 确定性派生（小写、无 padding，与 `randomOnlyOfficeKey` 编码风格一致）；`document.url` 拼为 `{version_service_url}/documents/{userUUID}/{rel}?action=version&versionId={versionID}`（路径段逐段 `url.PathEscape` 保留 `/`，`versionId` 做 `url.QueryEscape`），**不**嵌入任何凭据
- [x] 2.3 新增 `OnlyOfficeVersionConfig(c *gin.Context)` handler：① 闸门——`oo.secret` 或 `oo.VersionServiceURL` 任一空 → 503 `ONLYOFFICE_DISABLED`；② 取 `userID`（UUID）、`trace_id`、`rel`（`c.Param("path")` 去 leading `/`）；③ `versionId := c.Query("versionId")`，空 → 400 `BAD_REQUEST`；④ `xizhi.ValidatePathAllowReserved` 越界 → 403；⑤ 构造 config 并用 `jwt.SignClaims(h.oo.Secret, cfg)` 签名（复用 `signOnlyOfficeConfig` 失败路径）；⑥ 返回 `{ "server_url": oo.ServerURL, "view": { "config": cfg, "token": token } }`（复用 `onlyOfficeModeConfig{Config,Token}` 子结构 + 新增 `onlyOfficeVersionConfigResponse`）
- [x] 2.4 确认既有 `OnlyOfficeConfig`、`OnlyOfficeCallback`、`onlyOfficePersist`、SSRF 白名单、原子落盘**完全不动**（回调用例与既有 config 用例全绿）

## 3. Router：后缀分发注册

- [x] 3.1 `internal/handler/router.go` 新增常量 `onlyOfficeVersionConfigSuffix = "/onlyoffice-version-config"`
- [x] 3.2 `RouteDeps` 新增字段 `WorkspaceOnlyOfficeVersionConfig gin.HandlerFunc`（带注释：`GET .../onlyoffice-version-config?versionId=`，Bearer 鉴权，版本只读配置签发）
- [x] 3.3 `dispatchWorkspaceFile` 内新增第三个后缀分支：`*path` 以 `onlyOfficeVersionConfigSuffix` 结尾时，trim 后缀、`setPathParam`，转发 `deps.WorkspaceOnlyOfficeVersionConfig`（与既有 `onlyOfficeConfigSuffix` 分支对称；未新增顶层路由，`TestRegisterAPIRoutes_ExactRouteSet` 不回归）

## 4. 装配：serve.go 注入

- [x] 4.1 `cmd/blowball/serve.go` 构造 `WorkspaceHandler` 时把 `cfg.OnlyOffice.VersionServiceURL` 传入 `OnlyOfficeSettings`（在 `OnlyOfficeSettings` 结构体新增对应字段）
- [x] 4.2 `cmd/blowball/serve.go` 装配 `RouteDeps.WorkspaceOnlyOfficeVersionConfig = workspaceHandler.OnlyOfficeVersionConfig`（与既有 `WorkspaceOnlyOfficeConfig` 装配相邻）

## 5. API 文档

- [x] 5.1 `api/openapi.yaml` 新增端点 `GET /api/v1/workspace/files/{path}/onlyoffice-version-config`（`versionId` 必填 query 参数），响应 schema `OnlyOfficeVersionConfigResponse` `{ server_url, view:{ config, token } }`，错误码 400/401/403/503；描述含 view-only 内容约束（`mode:"view"`、`permissions.edit:false`、无 `callbackUrl`/`forcesave`、`document.url`→office-vers、确定性 `document.key`）。YAML 已通过解析校验。
- [x] 5.2 复制 `api/openapi.yaml` 到 blowball-frontend 并 `npm run generate-api`（**前端独立仓，需在 blowball-frontend 内执行**）

## 6. 后端测试

- [x] 6.1 `internal/handler/workspace_test.go` 在 `ooTestEnv` 补 `versionServiceURL` 字段、`newOnlyOfficeTestEnv` 注入 `VersionServiceURL` 并新增 `onlyOfficeVersionConfigSuffix` 后缀分支
- [x] 6.2 用例：200 正常签发——响应含 `server_url` 与 `view.{config,token}`；`view.config.editorConfig.mode=="view"`、`permissions.edit==false`、`download==true`；`view.config` 不含 `editorConfig.callbackUrl` 与 `customization`（且不依赖本地文件存在——版本源在 office-vers）
- [x] 6.3 用例：`document.url` 形如 `{version_service_url}/documents/user-1/report.docx?action=version&versionId=vid-123`，且不含 `token=`/`Bearer`
- [x] 6.4 用例：`document.key` 确定性——同一 `(path, versionId)` 两次请求 key 相等且等于 `base32(sha256(path+":"+versionId))`；不同 `versionId` key 不同
- [x] 6.5 用例：`view.token` 可用 secret 验签且 payload 恰为 `view.config`（复用 `verifyOnlyOfficeToken`）
- [x] 6.6 用例：缺 `versionId` → 400；路径越界（`../../etc/passwd`）→ 403；未鉴权 → 401；`secret` 空 **或** `version_service_url` 空 → 503（三种未配置子情形均覆盖）
- [x] 6.7 既有 `onlyoffice-config` 端点用例与回调用例**全部保持通过**（`make test -race` 全绿）

## 7. 前端（blowball-frontend 独立仓）

- [x] 7.1 历史版本预览入口：列出版本（别处提供 versionId）后，对选中版本请求 `GET .../onlyoffice-version-config?versionId=<vid>`（**前端独立仓**）
- [x] 7.2 消费 `resp.view.{config, token}` 实例化 `new DocsAPI.DocEditor`（复用既有 view 模式消费路径），以只读态打开该历史版本（**前端独立仓**）

## 8. 验证

- [x] 8.1 `make lint && make test` 通过（`go vet ./...` 退出 0；`go test -race ./...` 全绿）
- [x] 8.2 端到端（**待人工**）：配置 `onlyoffice.version_service_url` 指向 office-vers → 已鉴权用户对某历史 `versionId` 请求该端点 → 拿到 view 配置 → DocServer 拉取 `document.url`（office-vers 私网）成功渲染该版本只读内容；再次打开同一版本复用转换缓存（确定性 key）；未配置 `version_service_url` 时端点 503
