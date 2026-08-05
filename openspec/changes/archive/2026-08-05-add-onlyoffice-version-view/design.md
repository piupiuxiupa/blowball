# Design — OnlyOffice 历史版本只读配置端点

## Context

当前 OnlyOffice 集成（`office-file-editing` capability）面向**工作区内的活动文件**：

```
GET /api/v1/workspace/files/*path/onlyoffice-config
  → { server_url, edit:{config,token}, view:{config,token} }
  → document.url  = internal_backend/.../download/<path>?token=<JWT>   （blowball 是文件来源）
  → document.key  = 每次请求随机（强制重转、避免 stale cache）
  → callbackUrl   = internal_backend/.../onlyoffice-callback           （保存回写）
```

新增的 `office-vers` 是独立服务（MinIO 原生 S3 Object Versioning，MVP **无鉴权**，私网部署），以 `GET /documents/{uuid}/{path}?action=version&versionId=<vid>` 流式返回某文件某一历史版本的字节。文件按调用方提供的 `{uuid}`（本集成取**已鉴权用户的 user_id**，本身即 UUID）做命名空间隔离；同一逻辑路径每次上传产生新版本，历史版本**不可变**（只能追加或回滚产生新 latest，不能原地改写）。

要在编辑器内**只读浏览某一历史版本**，需要把 `document.url` 指向 office-vers 该版本、用确定性 key 复用转换缓存、且仅 view 模式。这与现有端点在三个维度上本质不同（文件来源、key 策略、模式集合），故新增专用端点而非扩展现有端点。

## Goals / Non-Goals

**Goals:**
- 新增 `GET .../onlyoffice-version-config?versionId=<vid>`，签发一份 view-only DocEditor 配置，`document.url` 指向 office-vers 该历史版本。
- `document.key` 按 `(path, versionId)` 确定性派生，复用 OnlyOffice 转换缓存。
- 响应结构仿照现有 `{server_url, view:{config,token}}`，前端复用 `resp.view.*` 消费路径。
- 复用现有签名、路径校验、闸门基建；零行为变更（新增配置字段默认空 → 端点 503）。

**Non-Goals:**
- 不做列版本历史、回滚、上传版本（由别处掌控 office-vers 数据面）。
- 不对 `versionId` 是否真实存在做主动校验（懒校验）。
- 不做 edit/review 等其他模式（历史版本不可变）。
- 不代理 office-vers、不在 blowball 内暴露 office-vers 地址管理。

## Decisions

### 决策 1：新增专用端点（后缀分发），而非扩展现有端点

`.../onlyoffice-config` 与 `.../onlyoffice-version-config` 同属工作区 GET catch-all 的后缀分发（新增 `onlyOfficeVersionConfigSuffix = "/onlyoffice-version-config"`，作为 `dispatchWorkspaceFile` 的第三个分支），Bearer JWT 鉴权，注册在 API 分区——与既有 `/onlyoffice-config`、`/content` 完全对称。

**为何不扩展现有端点（如加 `?versionId=`）**：现有端点的 `document.url`/`callbackUrl`/随机 key 三者均围绕"活动文件 + 可保存"设计，版本预览在这三点上全部相反（office-vers 来源、无回调、确定性 key）。混用会让一个端点承担两套互斥语义，且响应结构（双模式 vs 单 view）也不同。专用端点职责单一、回归面为零。

### 决策 2：`document.url` 指向 office-vers，不嵌入凭据

```
document.url = {onlyoffice.version_service_url}/documents/{userUUID}/{path}?action=version&versionId={vid}
```

office-vers 按设计无鉴权（README 明示私网部署），故**不**在 url 上嵌入用户 JWT——这与现有端点把 JWT 嵌入 `document.url`（因为目标是 blowball 自己的鉴权下载）根本不同。url 实质是**能力型 URL**：`userUUID` 与逻辑 `path` 本身非密，真正的不可猜测凭据是 `versionId`（MinIO 分配的 128-bit UUID）。拉取方是 DocumentServer（服务端跳），依赖 DocServer ↔ office-vers 处于私网。

### 决策 3：`document.key` 按 `(path, versionId)` 确定性派生

```
key = base32(sha256(path + ":" + versionId))   // 小写、无 padding，与 randomOnlyOfficeKey 编码一致
```

这是对现有端点"每次请求随机 key"策略的**有意背离**。理由：

| 场景 | 随机 key | 确定性 key（本决策） |
|---|---|---|
| 同一版本重复打开 | 每次重转 | 复用 OnlyOffice 缓存 ✅ |
| 不同版本 | 不同（无所谓） | 不同 ✅ |
| 同版本多用户共享 | 多次转换 | 一次转换共享 ✅ |

合法性根基：历史版本**不可变**（MinIO 版本化语义），同一 `(path, versionId)` 永远对应同一字节，确定性 key 不会命中 stale 内容。`versionId` 为 128-bit UUID，`:` 分隔符仅装饰性（versionId 不含 `:`，无碰撞）。base32(sha256) ≈ 52 字符，远低于 OnlyOffice 128 字符上限。

**不变式**：`document.url` 所用的 `path` 与 key 派生所用的 `path` **必须是同一字符串**，且须与 office-vers 实际存储该版本的逻辑路径一致。实现中两者均由同一 `rel`（经 `ValidatePathAllowReserved` 校验后的工作区相对路径）派生，天然一致；spec 以注释固化此约束。

### 决策 4：响应结构 `{server_url, view:{config,token}}`

仿照现有嵌套约定（`{config, token}` 包在模式键下、`server_url` 顶层共享），但**只返回 view 一种模式**。这样前端可复用既有的 `resp.view.config` / `resp.view.token` 消费与 DocEditor 实例化代码路径，无需新分支。

**被否决：返回完整双模式 `{server_url, edit, view}`**——历史版本无 edit 语义，返回伪造的 edit config 既不诚实也易误用。

**被否决：扁平 `{server_url, config, token}`**——破坏与现有端点的结构对称，前端需区分两种消费路径，收益为负。

### 决策 5：不含 `callbackUrl`

历史版本不可变，**无保存语义、无落盘目标**（office-vers 版本只能追加/回滚，不能原地覆盖）。现有 view config 保留 `callbackUrl` 是因为面向活动文件、结构对称且"零成本"；本端点连对称对象都不存在，省略 `callbackUrl` 更诚实，也略微收紧契约（DocumentServer 不会为本会话 POST 保存回调）。`customization.forcesave` 同理省略。

### 决策 6：懒校验 `versionId`

签发配置时**不**调用 office-vers `?action=versions` 确认 `versionId` 存在。理由：① 与"blowball 仅作签发方"定位一致；② 避免在配置构建路径引入新的外部往返与失败模式；③ 版本不存在时 DocumentServer 拉取 `document.url` 会得到 office-vers 的 404，错误自然浮现。`versionId` 仅做"非空"校验（缺失 → 400）。

### 决策 7：配置闸门 = secret AND version_service_url 非空

现有端点以 `onlyoffice.secret` 非空为"已配置"闸门。本端点需要 secret（签名）**与**新增的 `onlyoffice.version_service_url`（拼 `document.url`）同时非空；任一为空 → 503 `ONLYOFFICE_DISABLED`（复用同一 error code 与语义：未配置即不可用）。`version_service_url` 支持 `${VAR}` 展开，与所有配置项一致。

## Risks / Trade-offs

- **[office-vers 无鉴权 + 能力型 URL]** → 依赖私网部署 + `versionId`（128-bit UUID）作为不可猜测凭据。`document.url` 会出现在签发的 config 里（前端通过 DocEditor 间接持有），但前端本就需要 `versionId` 才能调用本端点，无额外泄露面。缓解：文档要求 office-vers 仅私网可达。
- **[path 一致性不变式]** `document.url` 与 key 派生所用 `path` 必须等于 office-vers 存储该版本的逻辑路径 → 实现中两者同源于 `rel`，但**跨服务一致性**取决于写入口径（非本端点职责）。缓解：spec 注释固化不变式；若未来写入口径与工作区 `rel` 不一致，需在写入侧对齐。
- **[确定性 key 与 OnlyOffice key 保留策略]** OnlyOffice 按自身策略清理转换缓存，确定性 key 不改变其清理行为，仅保证"命中时复用"。无额外风险。
- **[503 歧义]** secret 空与 version_service_url 空同返 503 `ONLYOFFICE_DISABLED`，运维需结合日志区分。可接受（与现有端点 503 语义一致：未配置即不可用）。

## Migration Plan

纯增量、零行为变更：
1. 新增配置字段 `onlyoffice.version_service_url`（默认空）。
2. 部署：未配置该字段时新端点返回 503，既有端点与回调用例完全不受影响。
3. 回滚：停止调用新端点即可；可保留代码（无害，未配置即 503）。

## Open Questions

无（范围已锁定：仅本端点，接收 `versionId`、返回签名的 view 配置；列版本/回滚由别处掌控）。
