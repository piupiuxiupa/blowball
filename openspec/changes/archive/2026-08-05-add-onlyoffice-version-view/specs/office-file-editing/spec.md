## ADDED Requirements

### Requirement: 签发已签名的 OnlyOffice 历史版本只读配置
系统 SHALL 提供端点 `GET /api/v1/workspace/files/*path/onlyoffice-version-config?versionId=<vid>`（与 `/onlyoffice-config`、`/content` 同属工作区 GET catch-all 的后缀分发，使用 Bearer JWT 鉴权），为指定办公文件的**某一历史版本**返回**只读** DocEditor 配置。响应 body SHALL 为 `{ "server_url": <string>, "view": {"config": <object>, "token": <string>} }`（仿照既有端点的嵌套约定，但**仅含 view 一种模式**），其中：

- `server_url` SHALL 为配置的 `onlyoffice.server_url`；
- `view.config` SHALL 为 DocEditor 配置对象，含 `documentType`（按扩展名 word/cell/slide）、`document{ fileType, key, title, url, permissions:{ edit:false, download:true } }`、`editorConfig{ mode:"view", user }`，且**不含** `editorConfig.callbackUrl`、**不含** `editorConfig.customization.forcesave`；
- `document.url` SHALL 拼接为 `{onlyoffice.version_service_url}/documents/{userUUID}/{path}?action=version&versionId={vid}`，其中 `{userUUID}` 为已鉴权用户的 `user_id`（UUID，作为 office-vers 命名空间），`{path}` 为经校验的工作区相对路径；该 url SHALL **不**嵌入任何鉴权凭据（office-vers 按设计无鉴权，依赖私网部署与不可猜测的 `versionId`）；
- `document.key` SHALL 按 `(path, versionId)` **确定性派生**为 `base32(sha256(path + ":" + versionId))`（小写、无 padding）；同一 `(path, versionId)` SHALL 恒产生同一 key，不同 `versionId` SHALL 产生不同 key；
  > 不变式：`document.url` 所用 `path`、`document.key` 派生所用 `path` SHALL 为同一字符串（实现中均同源于经 `xizhi.ValidatePathAllowReserved` 校验后的工作区相对路径）。
- `document.title` SHALL 为 `path` 的 basename，`document.fileType` SHALL 为 `path` 扩展名（去点、小写）；
- `view.token` SHALL 为用配置的 `onlyoffice.secret` 对 `view.config` 做 HS256 签名的 JWT。

该端点 SHALL 要求 `onlyoffice.secret` **与** `onlyoffice.version_service_url` 同时非空；任一为空时返回 503。`*path` SHALL 经 `xizhi.ValidatePathAllowReserved` 校验并限定在已鉴权用户工作区内。`versionId` 为必填 query 参数，缺失时返回 400。该端点 SHALL **不**主动校验 `versionId` 是否真实存在于 office-vers（懒校验——版本不存在时由 DocumentServer 拉取 `document.url` 时由 office-vers 返回 404）。该端点 SHALL **不**涉及任何落盘或回调。

#### Scenario: 已鉴权用户获取历史版本只读配置
- **WHEN** 已鉴权用户对工作空间内某办公文件发送 `GET /api/v1/workspace/files/<path>/onlyoffice-version-config?versionId=<vid>`
- **THEN** 系统返回 HTTP 200，body 为 `{ server_url, view:{ config, token } }`，`view.config.editorConfig.mode` 为 `"view"`，`view.config.document.permissions.edit` 为 `false`、`download` 为 `true`，且 `view.config` 不含 `editorConfig.callbackUrl` 与 `editorConfig.customization.forcesave`

#### Scenario: document.url 指向 office-vers 且不含凭据
- **WHEN** 检查返回的 `view.config.document.url`
- **THEN** url 形如 `{onlyoffice.version_service_url}/documents/{userUUID}/{path}?action=version&versionId=<vid>`，其中 `{userUUID}` 为该用户 `user_id`，且 url 中**不含**任何 `token`/JWT 凭据

#### Scenario: document.key 按版本确定性派生且可复用
- **WHEN** 对同一 `(path, versionId)` 连续两次请求该端点
- **THEN** 两次返回的 `view.config.document.key` 完全相等，且等于 `base32(sha256(path + ":" + versionId))`（小写、无 padding）

#### Scenario: 不同版本产生不同 key
- **WHEN** 同一 `path` 分别以 `versionId=A` 与 `versionId=B`（A≠B）请求
- **THEN** 两次返回的 `view.config.document.key` 不同

#### Scenario: view token 可被 DocumentServer 验签
- **WHEN** 把 `{...view.config, token}` 交给 `new DocsAPI.DocEditor(...)`
- **THEN** DocumentServer 用配置的 `onlyoffice.secret` 验签通过，编辑器以只读模式正常打开（JWT 校验不报错）

#### Scenario: 缺少 versionId 被拒绝
- **WHEN** 请求未携带 `versionId` query 参数
- **THEN** 系统返回 HTTP 400，body 包含 `BAD_REQUEST`

#### Scenario: 目标路径越界被拒绝
- **WHEN** `path` 解析后超出工作空间
- **THEN** 系统返回 HTTP 403，body 包含 `FORBIDDEN`

#### Scenario: 未鉴权请求被拒绝
- **WHEN** 请求未携带有效 Bearer JWT
- **THEN** 系统返回 HTTP 401

#### Scenario: OnlyOffice 或版本服务未配置
- **WHEN** `onlyoffice.secret` 为空 **或** `onlyoffice.version_service_url` 为空
- **THEN** 系统返回 HTTP 503，body 包含 `ONLYOFFICE_DISABLED`
