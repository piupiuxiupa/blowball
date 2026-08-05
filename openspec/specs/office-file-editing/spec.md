# office-file-editing Specification

## Purpose

通过 OnlyOffice DocumentServer 编辑工作空间内的办公文件（docx/xlsx/pptx 等）并把保存结果落盘回用户工作区。后端签发用 secret 签名的编辑器配置（secret 永不下发浏览器），接收 OnlyOffice 的保存回调，在保存状态把结果文件原子写回工作空间。所有 OnlyOffice 相关参数（secret、server_url、internal_backend）由后端配置驱动，并对回调中的结果 url 做 host 白名单限制以缓解 SSRF。

## Requirements

### Requirement: 签发已签名的 OnlyOffice 编辑器配置
系统 SHALL 提供端点 `GET /api/v1/workspace/files/*path/onlyoffice-config`（与 `/content` 同属工作区 catch-all 的后缀分发，使用 Bearer JWT 鉴权），为指定办公文件**同时返回编辑与只读两套** DocEditor 配置。响应 body SHALL 为 `{ "server_url": <string>, "edit": {"config": <object>, "token": <string>}, "view": {"config": <object>, "token": <string>} }`，其中：

- `server_url` SHALL 为配置的 `onlyoffice.server_url`（浏览器据此加载 api.js）；
- `edit.config` 与 `view.config` SHALL 共享**同一个随机生成的 `document.key`**（同一文件、同一次打开、一个文档身份；`document.key` 每次请求随机生成，保证每次打开重转）；
- `edit.config` SHALL 为 DocEditor 配置对象，含 `documentType`（按扩展名 word/cell/slide）、`document{ fileType, key, title, url, permissions:{ edit:true, download:true } }`、`editorConfig{ mode:"edit", callbackUrl, customization:{ forcesave:true }, user }`；
- `view.config` SHALL 为 DocEditor 配置对象，含 `documentType`、`document{ fileType, key, title, url, permissions:{ edit:false, download:true } }`、`editorConfig{ mode:"view", callbackUrl, user }`，且**不含** `customization.forcesave`；
- `edit.config` 与 `view.config` SHALL 在 `documentType`、`document.{fileType,key,title,url}` 上取值相同（仅 `permissions` 与 `editorConfig` 因模式而异）；
- `document.url` 与 `editorConfig.callbackUrl` SHALL 用配置的 `onlyoffice.internal_backend` 作为 host 拼接，并附带该用户的有效鉴权凭据；
- `edit.token` 与 `view.token` SHALL 分别为用配置的 `onlyoffice.secret` 对**各自** config（`edit.config` / `view.config`）做 HS256 签名的 JWT，两个 token 互不相同。

`*path` SHALL 经 `xizhi.ValidatePath` 校验并限定在已鉴权用户工作区内；文件不存在时返回 404；指向目录时返回 400。

> 安全约束（前瞻，本次不实现门控）：当前工作区严格 per-user，所有已鉴权用户均为文件 owner，edit/view 两套 token 同时下发给 owner 即安全。若未来引入文件共享或只读 ACL，edit token SHALL 按权限条件性下发（无编辑权限者仅返回 view）。

#### Scenario: 已鉴权用户同时获取编辑与只读配置
- **WHEN** 已鉴权用户对工作空间内存在的办公文件发送 `GET /api/v1/workspace/files/<path>/onlyoffice-config`
- **THEN** 系统返回 HTTP 200，body 含 `server_url`、`edit`（edit 模式 config + token）、`view`（view 模式 config + token），两 config 共享同一随机 `document.key`，`document.url`/`callbackUrl` 以 `internal_backend` 为 host

#### Scenario: 编辑与只读 token 各自可被 DocumentServer 验签且互不相同
- **WHEN** 把 `edit` 与 `view` 各自的 `{...config, token}` 分别交给 `new DocsAPI.DocEditor(...)`
- **THEN** DocumentServer 用同一 secret 分别验签通过，两种模式编辑器均正常打开（JWT 校验不报错），且 `edit.token` ≠ `view.token`

#### Scenario: 只读配置的内容约束
- **WHEN** 检查返回的 `view.config`
- **THEN** `editorConfig.mode` 为 `"view"`，`document.permissions.edit` 为 `false`、`download` 为 `true`，且**不含** `editorConfig.customization.forcesave`

#### Scenario: 编辑与只读共享同一随机 key
- **WHEN** 比较 `edit.config.document.key` 与 `view.config.document.key`
- **THEN** 两者相等（同一请求内共享一个随机生成的 key）

#### Scenario: 目标路径越界被拒绝
- **WHEN** `path` 解析后超出工作空间
- **THEN** 系统返回 HTTP 403，body 包含 `FORBIDDEN`

#### Scenario: 文件不存在
- **WHEN** `path` 指向的文件在工作空间内不存在
- **THEN** 系统返回 HTTP 404，body 包含 `NOT_FOUND`

#### Scenario: 路径指向目录
- **WHEN** `path` 指向一个目录
- **THEN** 系统返回 HTTP 400，body 包含 `BAD_REQUEST`

#### Scenario: 未鉴权请求被拒绝
- **WHEN** 请求未携带有效 Bearer JWT
- **THEN** 系统返回 HTTP 401

#### Scenario: OnlyOffice 未配置
- **WHEN** `onlyoffice.secret` 为空（未配置）
- **THEN** 系统返回 HTTP 503，body 包含 `ONLYOFFICE_DISABLED`

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

### Requirement: 接收 OnlyOffice 保存回调
系统 SHALL 提供端点 `POST /api/v1/workspace/onlyoffice-callback?path=<encoded>&token=<user_jwt>`，接收 OnlyOffice DocumentServer 在文档保存时发出的 JSON 回调（body 至少含 `status`、`key`、`url`）。该端点 SHALL 通过 URL query 中的用户 JWT 鉴权（复用既有 query-token 校验），并将 `path` 限定在已鉴权用户工作空间内（复用 `xizhi.ValidatePath`）。所有处理结果 SHALL 以 OnlyOffice 约定的 `{"error": <int>}` 响应（`0` 成功，非 `0` 触发重试）。

#### Scenario: 缺少 token 被拒绝
- **WHEN** 回调未携带 `token` query 参数
- **THEN** 系统返回 HTTP 401，body 包含 `missing token`

#### Scenario: 无效或过期 token 被拒绝
- **WHEN** `token` 签名错误、格式非法或已过期
- **THEN** 系统返回 HTTP 401，body 包含 `invalid token` 或 `token expired`

#### Scenario: 目标路径越界被拒绝
- **WHEN** `path` 解析后超出工作空间
- **THEN** 系统返回 HTTP 403，body 包含 `FORBIDDEN`

### Requirement: 在保存状态把文档落盘
当回调 `status` 为 `2`（ready for saving）或 `6`（force save）且 body 含 `url` 时，系统 SHALL 从该 `url` 下载编辑后的文件，并写回 `data/{userID}/workspace/{path}`（覆盖原文件），随后返回 `{"error": 0}`。

#### Scenario: 保存回调（status=2）
- **WHEN** 回调 `status=2` 且 `url` 指向 DocumentServer 上的结果文件
- **THEN** 系统下载该 `url`、覆盖写回工作空间对应路径，返回 `{"error": 0}`

#### Scenario: 强制保存回调（status=6）
- **WHEN** 回调 `status=6`（forcesave）且 `url` 存在
- **THEN** 系统下载该 `url`、覆盖写回工作空间对应路径，返回 `{"error": 0}`

### Requirement: 非保存状态仅确认不落盘
当回调 `status` 为 `1`/`3`/`4`/`7` 时，系统 SHALL 不下载、不写文件，直接返回 `{"error": 0}`。

#### Scenario: 开始编辑（status=1）
- **WHEN** 回调 `status=1`
- **THEN** 系统不修改工作空间文件，返回 `{"error": 0}`

#### Scenario: 关闭且无改动（status=4）
- **WHEN** 回调 `status=4`
- **THEN** 系统不修改工作空间文件，返回 `{"error": 0}`

### Requirement: 原子写回
落盘 SHALL 采用"先写同目录临时文件、成功后再 `rename` 覆盖目标"的方式，确保下载中断或写入失败时**原文件不被破坏**；失败时返回 `{"error": 1}` 并记录日志。

#### Scenario: 下载结果文件失败时原文件不变
- **WHEN** 保存状态下载 `url` 失败（网络错误、非 2xx）
- **THEN** 工作空间原文件内容不变，返回 `{"error": 1}` 并记录告警

#### Scenario: 写临时文件失败时原文件不变
- **WHEN** 临时文件写入或 `rename` 失败
- **THEN** 工作空间原文件内容不变，返回 `{"error": 1}` 并记录告警

### Requirement: 限制可下载的结果 url 来源（缓解 SSRF）
系统 SHALL 仅下载 host 与配置的 `onlyoffice.server_url` 的 host 匹配的 `url`；不匹配时 SHALL 拒绝下载、不写文件，返回 `{"error": 1}` 并记录日志。下载字节数超过 `maxUploadBytes` 时亦 SHALL 拒绝。

#### Scenario: 结果 url 指向已配置的 DocumentServer
- **WHEN** 回调 `url` 的 host 等于 `onlyoffice.server_url` 的 host
- **THEN** 系统正常下载并落盘

#### Scenario: 结果 url 指向未授权的外部地址
- **WHEN** 回调 `url` 的 host 不等于 `onlyoffice.server_url` 的 host
- **THEN** 系统拒绝下载、不写文件，返回 `{"error": 1}` 并记录告警
