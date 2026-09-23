# Design: add-turn-artifacts

## Context

blowball 已有完整的每用户工作空间文件服务（List/Search/Download/Content、token 下载、OnlyOffice 编辑与历史版本查看配置）。缺口在 agent 最终回复（纯 markdown 文本）与前端工作区 UI 之间：文件引用不可点击、历史引用随文件被覆盖而失真。

关键现状约束（探索阶段确认）：

- **office 交付物主要由 bash 产出**：`.docx/.xlsx` 等二进制文件是 bash 沙箱里跑 python 脚本生成的，不经过 `xizhi_*` 写工具——写路径钩子无法覆盖全部产物来源。
- **持久化是事件流**：消息按事件行持久化（token/tool_call/tool_result…），`done` 事件本身**不持久化**（仅实时流）。因此 version_id 必须放在可持久化的事件里。
- **OnlyOffice config 接口已同时返回 Edit/View 两套签名配置**："点击默认预览"是前端选择 View 的问题，后端零改动。
- **office-vers 是外部服务**（MinIO 背书，无鉴权），提供完整版本读写 API（`POST /documents/{uuid}/{path}` 上传新版本，`?action=version&versionId=` 读指定版本）。本设计用它做唯一版本存储，与 OnlyOffice 编辑路径共用同一版本空间。

## Goals / Non-Goals

**Goals:**
- 最终回复中的交付物引用可点击，点击后打开对应版本（office 默认预览）。
- 产物按 turn 聚合，前端可渲染产物 chip 行/面板。
- 旧消息中的引用打开旧版本（语义：该 turn 结束时的文件状态）。
- 覆盖 xizhi 与 bash 两种产物来源。

**Non-Goals:**
- 前端实现（另见 frontend-handoff.md）。
- 版本库与 office-vers 合并（外部服务，保持现状）。
- 产物删除事件的版本快照（被删文件的旧版本仍在库中，旧链接不失效；"当前版本"404 为可接受行为）。
- turn 内实时产物事件（v1 统一在 turn 末发出；后续可从 xizhi 工具层加实时事件）。
- 版本库容量治理/保留策略（记录为后续工作）。

## Decisions

### D1: 版本模型 = turn 末快照，而非覆盖前抢救

"旧链接指向旧版本"用**事后快照**实现：turn 结束时把本轮产物的最终内容复制进版本库。消息 M（turn T）中引用的文件，其版本即 turn T 结束时的状态。

对比"覆盖前抢救"（写路径挂钩子，覆盖前存旧版）：

```
  覆盖前抢救                    turn 末快照（采用）
 ┌──────────────┐            ┌──────────────┐
 │ 需拦截每个写路径 │            │ 与写路径解耦    │
 │ xizhi ✓      │            │ xizhi ✓      │
 │ bash  ✗✗✗    │            │ bash  ✓      │
 │ MCP工具 ✗     │            │ 任何来源 ✓    │
 │ 同turn多次写→  │            │ 同turn多次写→ │
 │  语义模糊      │            │  只存最终态 ✓  │
 └──────────────┘            └──────────────┘
```

bash 沙箱内写文件对后端不可见（bind mount 直写），覆盖前抢救对 bash 在工程上不可行；turn 末快照天然覆盖所有来源。代价：版本粒度是"turn 末状态"而非"每次写入"——与"按 turn 聚合"的需求恰好一致。

### D2: 产物检测 = turn 末 mtime 扫描

turn 开始时记录 `turnStart`；turn 结束时遍历用户工作空间，收集 `mtime >= turnStart` 的常规文件，排除 `tmp/`、`.blowball/`、隐藏目录（`.pip` 等沙箱支持目录同为隐藏目录，一并排除）。

- 风险：脚本显式保留 mtime（`touch -r`、某些解压）会漏检——可接受，v1 记录为已知限制。
- 性能：扫描为单用户目录树 walk，turn 末一次；大工作区（万级文件）耗时在毫秒~百毫秒级，可接受。若成为瓶颈，可加 `since` 增量目录缓存。

### D3: 链接语法 = 自定义 scheme `blowball://workspace/<path>`，消息文本不落版本参数

模型只写无参数链接；版本钉定（vid）由系统侧数据提供：

- **同 turn 引用**（主路径）：turn 末 artifact 事件携带 `(path, vid)`，前端渲染消息时按 path 匹配本 turn 产物列表完成钉版。
- **跨 turn 引用**（兜底）：前端调 `versions/resolve?path=P&before=<消息时间>`。

备选方案"后端在持久化时改写消息文本、把 vid 钉进链接"被否决：持久化单元是 token 事件行，改写要回写已刷盘的 token 行，复杂且易与 msgflush 写回批次竞争；而且文本保持无版本对 LLM 上下文更干净（重建时模型看到的链接与产出时一致）。

自定义 scheme 而非裸 API 路径的理由（同 ChatGPT `sandbox:`）：意图无歧义、前端有单一拦截点、不与普通 URL/文本路径混淆。前端 markdown 渲染器需将该 scheme 加入 allowlist（默认 sanitizer 会剥掉未知 scheme）。

### D4: artifact 事件 = turn 末批量发出，随事件流持久化；done.meta 仅作实时摘要

时序：

```
 agent 输出 tokens ──▶ [turn末: 扫描+快照] ──▶ artifact×N ──▶ done(meta.artifacts)
                            │
                            └─ 版本库写入 + 索引记录
```

- artifact 事件是普通流事件，经 TurnEventTap 进入现有持久化管道，历史重放时前端可还原产物列表——**这是 vid 的持久化载体**（done 不持久化）。
- 挂载点：`TurnHooks` 新增 `FinalizeTurn(ctx) []stream.ArtifactInfo`（由 message_stream 构建的闭包，捕获 userID/workspaceRoot/turnStart）。实现在 **orchestratorAdapter 的 done 分支**注入：收到 done 后先调 FinalizeTurn，artifact 事件先于 done 进入外层 hub（实时流/运行日志）并追加进持久化事件切片，done.Meta 附摘要。agent 包零改动，不感知文件系统。
- 消息重建（`MessagesToAgentMessages`）跳过 artifact 事件类型，不进 LLM 上下文。

### D5: 版本库存储 = office-vers 服务 + MySQL 索引（修订：曾用本地盘 blob，已删）

- 存储：`POST {version_service_url}/documents/{userID}/{path}`（裸字节）→ office-vers 返回 MinIO version id。复用现有 `onlyoffice.version_service_url` 配置，无新配置项、无新存储系统。
- 索引：新表 `file_versions(id PK, user_id, path, version_id, size, mime, sha256, create_time)`——ownership、as-of 解析（按 create_time 排序，不依赖 vid 有序）、sha256 去重都由它承担；office-vers 只是字节仓库。
- 去重在我们侧：office-vers 每次 POST 都生成新版本，所以 sha256 与索引最新版一致时不上传。
- 未配置 version_service_url → store 为 nil，产物照常发事件但无 vid（与 OnlyOffice 未配置的 503 降级语义一致）。
- office-vers 无鉴权（其文档明示）：只允许内网服务到服务访问；前端永远拿不到 office-vers URL，非 office 读取由 blowball 代理。

### D6: 版本读取与预览接口复用既有模式

- `GET /api/v1/workspace/versions/{vid}/content`：Bearer 或 `?token=` 鉴权，后端代理从 office-vers 取字节（供 `<img>`/iframe/非 office 预览与下载）。
- office 历史版本预览：**零新增**——直接复用现有 `GET /api/v1/workspace/files/*path/onlyoffice-version-config?versionId=`（其 document.url 本就指向 office-vers，版本入库后它自然可用）。
- vid→(user,path) 绑定只从索引查，路径遍历攻击面为零（vid 是不透明 id）。

### D7: office 点击默认预览 = 前端默认消费 View 配置

现有 `onlyoffice-config` 响应已含 `Edit`/`View` 两套签名配置，前端默认用 View 即可；历史版本走 D6 的版本预览配置（本就只读）。后端无编辑逻辑改动。

## Risks / Trade-offs

- [mtime 检测漏掉保留时间戳的写入] → 文档化为已知限制；prompt 不引导此类操作；后续可加写入工具层的实时事件补盲。
- [模型不写约定链接（依从性）] → artifact 事件/产物面板不依赖文本链接，是确定性兜底；链接依从性问题只影响叙事内联体验，不影响产物可发现性。
- [turn 末扫描+快照增加 turn 尾延迟] → 仅在有产物时发生；快照为本地文件复制，设单文件大小上限（配置项，默认 200MB），超限跳过该文件快照（事件仍发，vid 缺省，前端按当前版本处理）。
- [版本库无界增长] → office-vers 侧按桶生命周期/保留策略治理（S3 版本治理是成熟能力），blowball 不管。
- [artifact 事件进 LLM 上下文污染对话] → D4 明确重建跳过。

## Migration Plan

1. 新 migration：`file_versions` 表。
2. 新增配置项（单文件快照上限），`config.example.yaml` 同步；版本库复用 `onlyoffice.version_service_url`。
3. 后端实现（见 tasks.md），`api/openapi.yaml` 同步新接口。
4. 前端按 frontend-handoff.md 接入。
5. 回滚：功能无侵入存量接口，下线新路由+回滚 migration 即可；已产出的版本库目录可保留。

## Open Questions

- 产物面板在会话维度的聚合视图（跨 turn "本会话全部产物"）是否值得做，v1 仅按 turn。
