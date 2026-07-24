# Design — OnlyOffice 编辑/只读双模式配置

## 背景：为什么必须有两个 token

OnlyOffice 在 `local.json` 开启 `token.enable` 后，DocumentServer 会对浏览器送来的 editor config 做 JWT 验签：

```
浏览器: new DocsAPI.DocEditor(id, { ...config, token })
                                                        │
                                                        ▼
DocumentServer: 重新算 HS256(secret, config)  ==  token ?
                                                        │
                                              不等 → JWT 校验失败，拒开
```

`token` 签的是**整个 config 对象**（`{documentType, document, editorConfig}`）。因此前端**不能**在拿到 edit 配置后自行修改 `mode`/`permissions` 再复用同一个 token——任何 config 字段变动都让签名失效。

结论：**每一种互不相同的 config 都需要一个后端独立签名的 token**。编辑与只读是两套不同 config，故必须签两个 token。

## edit 与 view config 的差异

| 字段 | edit（既有） | view（新增） |
|---|---|---|
| `editorConfig.mode` | `"edit"` | `"view"` |
| `document.permissions.edit` | `true` | `false` |
| `document.permissions.download` | `true` | `true`（保留，允许下载） |
| `editorConfig.customization.forcesave` | `true` | **省略**（只读无保存） |
| `editorConfig.callbackUrl` | 有 | **保留**（见下） |
| `documentType` / `document.{fileType,title,url}` | — | 与 edit **完全相同** |
| `document.key` | 随机 | **与 edit 共享同一随机值**（见下） |
| `token` | `HS256(secret, editConfig)` | `HS256(secret, viewConfig)`（不同） |

## 决策 1：两套 config 共享同一个随机 `document.key`

`document.key` 是 config **内部**的一个普通标识字段（DocumentServer 据此标识/缓存已转换文档），**不是**安全凭据——安全边界是签名后的 `token`。`randomOnlyOfficeKey()` 已保证每次请求生成新随机值、每次打开强制重转。

同一请求里 edit/view 共用一个生成的 key，语义即「同一文件、同一次打开、一个文档身份」，比生成两个独立 key 更简洁。即便给两个不同 key 也无害（各自重转），但冗余。**采用共享一个 key。**

## 决策 2：响应体结构（嵌套对称，破兼容）

采用嵌套对称结构：

```json
{
  "server_url": "https://oo.example.com",
  "edit": { "config": { /* mode:edit ... */ }, "token": "<jwt-edit>" },
  "view": { "config": { /* mode:view ... */ }, "token": "<jwt-view>" }
}
```

前端按选定模式取 `edit` 或 `view`，用其 `{config, token}` 实例化 DocEditor。`server_url` 共享（同一个 DocumentServer）。

### 被否决的替代：增补式向后兼容（`config`/`token` 仍是 edit + 新增 `view_config`/`view_token`）

```
{ server_url, config, token, view_config, view_token }   // config/token = edit
```

- 优点：保留旧字段，前端可渐进迁移。
- 缺点：非对称、易误导（为何 edit 特殊？），未来若再加 review/comment 模式会越发混乱。
- 结论：blowball 后端与前端同属一人维护，端点为内部契约，**一次性切到对称嵌套结构更干净**，破兼容可接受。

### 被否决的替代：`?mode=edit|view` 查询参数

端点只签发请求的那一种模式，保持旧响应结构。
- 缺点：前端切换编辑/只读需**一次网络往返**重新取签名配置，正是本变更要消除的延迟。
- 结论：与目标相悖，不采用。

## 决策 3：view config 仍保留 `callbackUrl`

只读态（`mode:"view"`、`permissions.edit:false`）DocumentServer **不会**发出保存类 status（2/6），只会发出 status=1（打开）/4（关闭无改动）。既有 `OnlyOfficeCallback` handler 对非保存状态一律返回 `{"error":0}`、不落盘，因此把同一 `callbackUrl`（复用同一用户 JWT）放进 view config **零额外成本、零副作用**，且与 edit 保持结构对称、便于复用同一构建逻辑。

## 安全说明：为何现在不需要权限门控

工作区严格 per-user：`data/{userID}/workspace`，无文件共享/ACL，每个已鉴权用户都是其文件的 owner。因此把 edit token 与 view token 同时下发给 owner 是安全的——前端无法越权（两个 token 都只是对各自 config 的签名，无独立权限语义）。

**前瞻约束**：若未来引入文件共享或「只读访客」类 ACL，edit token **必须**按权限条件性下发（无编辑权限者只返回 view），否则只读用户可拿 edit token 开编辑态。本变更在 spec 中以注释固定此约束，但**不在本次实现**门控逻辑。

## 实现要点（不改回调/落盘）

- `buildOnlyOfficeConfig(rel, userJWT)` 拆出共享随机 key 与公共字段，产出 edit/view 两个 map。
- `OnlyOfficeConfig` handler 对两个 map 分别 `jwt.SignClaims(h.oo.Secret, cfg)`，组装嵌套响应。
- 503/403/404/400 错误路径、`xizhi.ValidatePath` 越界校验、文件存在性/目录判断全部不变。
- `OnlyOfficeCallback`、`onlyOfficePersist`、SSRF host 白名单、原子 rename 落盘**完全不动**。

## 测试策略

- 配置端点：正常返回含 `edit`+`view` 两套 `{config, token}`；两 token 均可被同一 secret 验签且 payload 恰为各自 config（**互不相同**）；edit/view 的 `mode`、`permissions.edit`、`forcesave` 存在性符合上表；共享同一 `document.key`；路径越界 403、文件不存在 404、目录 400、未鉴权 401、未配置 503。
- 回调端点：既有用例全部保持通过（不回归）。
