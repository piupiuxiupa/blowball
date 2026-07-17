## ADDED Requirements

### Requirement: 签发已签名的 OnlyOffice 编辑器配置
系统 SHALL 提供端点 `GET /api/v1/workspace/files/*path/onlyoffice-config`（与 `/content` 同属工作区 catch-all 的后缀分发，使用 Bearer JWT 鉴权），为指定办公文件返回 OnlyOffice 编辑器所需的完整配置。响应 body SHALL 为 `{ "server_url": <string>, "config": <object>, "token": <string> }`，其中：
- `config` 为 DocEditor 配置对象，含 `documentType`（按扩展名 word/cell/slide）、`document{ fileType, key, title, url, permissions:{ edit:true, download:true } }`、`editorConfig{ mode:"edit", callbackUrl, customization:{ forcesave:true }, user }`；
- `document.url` 与 `editorConfig.callbackUrl` SHALL 用配置的 `onlyoffice.internal_backend` 作为 host 拼接，并附带该用户的有效鉴权凭据；
- `document.key` SHALL 为每次请求随机生成；
- `token` SHALL 为用配置的 `onlyoffice.secret` 对 `config` 做 HS256 签名的 JWT；
- `server_url` SHALL 为配置的 `onlyoffice.server_url`（浏览器据此加载 api.js）。
`*path` SHALL 经 `xizhi.ValidatePath` 校验并限定在已鉴权用户工作区内；文件不存在时返回 404。

#### Scenario: 已鉴权用户获取编辑配置
- **WHEN** 已鉴权用户对工作空间内存在的办公文件发送 `GET /api/v1/workspace/files/<path>/onlyoffice-config`
- **THEN** 系统返回 HTTP 200，body 含 `server_url`、`config`（edit 模式、随机 key、`document.url`/`callbackUrl` 以 `internal_backend` 为 host）、以及用 secret 签名的 `token`

#### Scenario: 配置签名可被 DocumentServer 校验通过
- **WHEN** 把返回的 `{...config, token}` 交给 `new DocsAPI.DocEditor(...)`
- **THEN** DocumentServer 用同一 secret 验签通过，编辑器正常打开（JWT 校验不报错）

#### Scenario: 目标路径越界被拒绝
- **WHEN** `path` 解析后超出工作空间
- **THEN** 系统返回 HTTP 403，body 包含 `FORBIDDEN`

#### Scenario: 文件不存在
- **WHEN** `path` 指向的文件在工作空间内不存在
- **THEN** 系统返回 HTTP 404，body 包含 `NOT_FOUND`

#### Scenario: 未鉴权请求被拒绝
- **WHEN** 请求未携带有效 Bearer JWT
- **THEN** 系统返回 HTTP 401

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
