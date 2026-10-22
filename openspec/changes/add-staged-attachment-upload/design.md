## Context

`add-image-attachments`（已完成、待归档）建立了会话作用域的图片附件链路：`POST /sessions/:session_id/attachments` 上传 → `data/{userID}/attachments/{sessionID}/{attachmentID}.{ext}` 落盘 + `attachments` 注册表行 → 发送时 `attachment_ids` 经 `(id, user_id, session_id)` 三元组校验 → 当前轮 inline base64 给模型、历史轮确定性占位符；删会话时归档行并 `RemoveAll` 会话目录。

该链路的硬约束是**上传必须发生在会话创建之后**：上传 URL 携带 `session_id` 且 handler 先做会话归属校验。新会话场景下客户端只能"建会话 → 串行传完全部图片 → 发首条消息"，弱网下用户在点击发送后长时间空等。

关键约束（继承自基线变更，不在本次重议）：

- 消息持久化走 write-behind，附件引用随消息行原子落库；base64 不落任何持久层。
- 附件文件不可变，重传 = 新 id；内容端点 `?token=<jwt>` + immutable 缓存。
- 读取失败一律降级为占位符 + ERROR 日志，turn 不中断。

## Goals / Non-Goals

**Goals:**

- 图片"选中即传"：上传不依赖会话存在，粘贴/选图后立即后台上传，发送时消息体只带 id
- 发送时把暂存附件原子绑定到目标会话，绑定后读取面（模型渲染、内容端点、历史）与现状字节级一致
- 暂存孤儿有确定的生命周期：TTL 清扫 + 每用户暂存配额
- 旧端点保留，客户端可灰度迁移

**Non-Goals:**

- 不改变已绑定附件的任何读取/渲染/缓存/删除语义
- 不做暂存附件的后端读取端点（composer 预览由前端本地 blob URL 承担）
- 不做"刷新后恢复 composer 暂存列表"的列表端点（前端重传即可，旧图由清扫回收）
- 不做服务端图片处理（沿用基线 Non-Goals）
- 配额/TTL/清扫周期为初版常量，不做配置化

## Decisions

### D1. 存储布局：暂存哨兵目录 + 绑定即 rename（选方案 A）

暂存文件落盘 `data/{userID}/attachments/_staging/{attachmentID}.{ext}`，`fs.Store` 新增 `UserAttachmentStagingDir(userID)`。`_staging` 不可能与 session id 冲突（session id 为 UUID v7）。发送认领成功后 `os.Rename` 进 `data/{userID}/attachments/{sessionID}/`——同文件系统 rename 是廉价原子操作。

**备选（否决）**：扁平用户级目录 `attachments/{id}.{ext}`，`session_id` 仅作行元数据。省掉 rename，但 `loadCurrentTurnImages`、内容端点、删会话 `RemoveAll` 三处读取/清理面全部要改为行驱动逐文件删；而 rename 方案让读取面**零改动**（绑定后路径形态与现状一致），删会话的 D8 四步流程原样成立。

### D2. 数据模型：`session_id` 可空 + 原子认领 UPDATE

migration 022：`attachments.session_id` 改为 NULLABLE（NULL = 已上传未绑定），`attachments_deleted` 镜像同步；新增 `KEY idx_session_created (session_id, create_time)` 供清扫扫描。存量行全部非 NULL，不受影响。

认领用数据库作 single source of truth：

```sql
UPDATE attachments SET session_id = ? WHERE id = ? AND user_id = ? AND session_id IS NULL
-- affected rows = 1 → 认领成功；= 0 → 被抢占/不存在/已绑定他人
```

**备选（否决）**：先 SELECT 再 UPDATE 的两步校验——存在 TOCTOU 并发窗，两个会话可同时"看到"未绑定。条件 UPDATE 单语句原子，无需事务。

### D3. 认领时机与顺序：校验阶段先 UPDATE 后 rename

`SendMessage` 现有附件校验位于 token 校验之后、占 turn 槽之前，认领与 rename 插入同一位置：

1. 按 `(id, user_id)` 查行；行不存在/属他人 → 404（现状语义）
2. 行 `session_id` = 本会话 → 放行（同会话重试/复用，幂等，无 rename）
3. 行 `session_id` 非空且 ≠ 本会话 → 404（跨会话引用已绑定附件）
4. 行 `session_id` 为 NULL → 条件 UPDATE 认领；失败 → 404；成功后 rename 暂存文件进会话目录

rename 失败（极窄不一致窗：行已绑定、文件在暂存）→ best-effort 将行 UPDATE 回 NULL + ERROR 日志，该附件按既有"文件缺失"路径降级为占位符，**turn 继续**（沿用基线"附件问题不中断 turn"原则）。回滚再失败则留已绑定孤儿文件，由运营清理，不产生正确性问题。

**备选（否决）**：先 rename 后 UPDATE——UPDATE 失败时文件已进会话目录而行未绑定，产生无行孤儿文件且方向相反、更难回收。数据库先行保证"行状态永远领先于文件位置"，回收方向（NULL 行 + 暂存文件）恰好是 TTL 清扫覆盖的形态。

### D4. 孤儿治理：TTL 清扫 + 暂存配额双闸

- **配额**：每用户暂存态（`session_id IS NULL`）附件上限 20 个。上传前 `COUNT(*)` 检查，超限返回 HTTP 429。计数与插入间的并发超额窄窗可接受（backstop 而非硬保证）。
- **清扫**：服务启动时注册后台 ticker goroutine（模式参照 `internal/run` drain keeper），每 10 分钟扫描 `session_id IS NULL AND create_time < now()-24h` 的行，逐行删文件 + 删行；单行失败仅记日志、下轮重试。只触碰暂存态，已绑定附件生命周期仍归删会话流程。

**备选（否决）**：复用删会话流程清理暂存——暂存附件不属于任何会话，无会话删除事件可挂；独立清扫是唯一覆盖"上传后从未发送"形态的机制。

### D5. 兼容别名：旧端点保留为"上传即绑定"

`POST /sessions/:session_id/attachments` 行为不变：会话归属校验后走同一上传核心（嗅探/写盘/插行），直接写会话目录、插入非 NULL 行。两个端点共享校验与落盘辅助函数，差异仅是目标目录与 `session_id` 初值。内容端点不变——暂存态行天然不满足三元组查询，`<img>` 读取返回 404（composer 预览走前端本地 blob URL）。

### D6. HTTP 契约

- `POST /api/v1/attachments`（Bearer，multipart `file`）：响应与旧端点同构 `{attachment_id, filename, mime, size}`；错误码沿用 `FILE_TOO_LARGE`(413) / `UNSUPPORTED_IMAGE_TYPE`(400)，新增 429（暂存配额超限）。
- `POST /sessions/:id/messages` 的 `attachment_ids`：接受本会话已绑定附件与暂存附件；其余校验（≤4 张、vision 能力位、token 上限）不变。
- `api/openapi.yaml` 同步。

## Risks / Trade-offs

- [认领 UPDATE 与 rename 两步不一致] → 数据库先行保证行状态领先；rename 失败 best-effort 回滚 + 占位符降级 + ERROR 日志；无正确性风险。
- [暂存上传面扩大导致孤儿堆积（无会话删除兜底）] → 24h TTL 清扫 + 每用户 20 个配额双闸；两个常量初版拍死，运行后按观测调整。
- [配额计数竞态导致轻微超额] → 接受：配额是成本 backstop，超额几个文件无安全/正确性影响。
- [弱网"上传成功但发送未到达"的暂存附件] → 24h 后清扫回收；前端刷新丢失 composer 引用后用户重传，旧图同样由清扫回收。
- [回滚时存量 NULL 行] → 旧代码的三元组查询天然不匹配 NULL 行（不可见），`_staging` 目录旧代码不触碰，代码回滚即可；列可保持可空无需回填。

## Migration Plan

1. 先归档/同步 `add-image-attachments`（本变更的 spec delta 以其 `image-attachments` delta 为基线）
2. 存量数据库手动执行 migration 022（`session_id` 改 NULLABLE + 新索引 + `attachments_deleted` 镜像同步）；Docker initdb 仅首次初始化生效
3. 发布代码：旧端点行为不变，新端点与清扫任务上线，前端可独立灰度切换"选中即传"
4. 回滚：直接回滚代码（见 Risks 末条）；如需彻底回退 schema，先清空暂存行再改回 NOT NULL

## Open Questions

- 配额 20 个 / TTL 24h / 清扫周期 10min 为初版常量，上线后按磁盘占用与命中率观测再调（暂不做配置化）
- 是否需要 staging 列表端点供刷新后恢复 composer——初版不做，待前端实际体验反馈再议
