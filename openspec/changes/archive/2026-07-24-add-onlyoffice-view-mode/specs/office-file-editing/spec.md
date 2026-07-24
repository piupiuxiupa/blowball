## MODIFIED Requirements

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
