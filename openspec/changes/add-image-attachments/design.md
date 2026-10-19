## Context

主对话管线目前是纯文本的：`sendMessageRequest` 只有 `content` 字符串（1MB body 上限 + token 估算上限），`agent.Message` 只有 `Content string`，历史每轮从 Redis/MySQL 全量重建回放，compaction 对中间历史做摘要。唯一的图片通路是 `xizhi_view_image` 副驾——agent 主动调用、仅限 workspace 图片、经 `LLMRequest.vision` 未导出侧信道把**单张**图片以 base64 `image_url` part 附在最后一个 user 消息上发给独立识图模型，文字描述回传主对话。`openai_client.withVisionImage` 已经具备 data URL content part 的渲染能力，可复用其形态但不能复用其"单图 + 附在最后一条 user 消息"的语义。

关键约束：

- 消息持久化走 write-behind（Redis ingest queue → MySQL flusher，client_msg_id 幂等去重），消息↔附件链接必须与消息行同生命周期，避免双写 split-brain。
- 历史回放遵循 append-only 字节契约（下一轮回放上一轮实发内容），这是模型侧前缀缓存可命中的前提。
- `llm_raw_log.raw` 为 MEDIUMTEXT（16MB 上限）且承诺不截断；多图 base64 必然超限，必须在捕获层脱敏。
- 附件内容端点要服务 `<img src>`，无法携带 Authorization 头；仓库已有 `QueryTokenAuthMiddleware`（`?token=<jwt>`，显式 opt-in 路由）与 TokenDownload / artifact version content 两个先例。

## Goals / Non-Goals

**Goals:**

- 用户粘贴图片 → 上传 → 随消息发送 → 模型直接看到原图（当前轮），全链路闭环
- base64 只存在于发往 LLM 的内存请求，任何持久层不落 base64
- 前缀缓存契约受控退化：每张图片只发生一次有界的渲染变化（inline → 占位符），此后字节稳定
- 附件所有权、会话生命周期、不可变缓存全部有服务端保证

**Non-Goals:**

- 不改动 `xizhi_view_image` 副驾（workspace 识图、独立识图模型的能力保持原样）
- 不做服务端图片处理（EXIF 剥离、压缩、转码、缩略图生成）；前端如需缩略图属后续独立变更
- 不做多图滑动窗口（K>1）——渲染策略实现为常量，K=1；窗口调大属后续调参
- 不做附件配额、孤儿定时清理的强一致性保证（表结构预留，清理 best-effort）
- 非图片文件附件（PDF/文档）不在本期

## Decisions

### D1. 存储：与 workspace 同级的 session 作用域目录

`data/{userID}/attachments/{sessionID}/{attachmentID}.{ext}`，`attachmentID` 为 UUID v7，扩展名来自嗅探后的 MIME。`fs.Store` 新增 `UserAttachment(sessionID)` 解析函数；`userSubDirs` 不加 attachments（它只描述用户根下的固定目录，attachments 按 session 惰性创建）。

**备选（否决）**：存 `workspace/uploads/` 内。虽可复用现有 upload/download/view_image 全套端点，但聊天附件与工作文件生命周期不同（随会话删除 vs 用户文件），混放导致文件树噪音、删除策略纠缠、agent 文件工具可误删聊天图片。代价只是几个新 handler。

### D2. 数据模型：注册表 + 消息行内引用，职责分离

```sql
-- migration 021
CREATE TABLE attachments (
  id         VARCHAR(36) PRIMARY KEY,   -- UUIDv7，即磁盘文件名主干
  user_id    VARCHAR(64) NOT NULL,
  session_id VARCHAR(64) NOT NULL,
  filename   VARCHAR(255) NOT NULL,     -- 原始文件名（展示用，不用于磁盘路径）
  mime       VARCHAR(64)  NOT NULL,     -- 魔数嗅探结果
  size       BIGINT       NOT NULL,
  message_id BIGINT       NULL,         -- best-effort 回填（发送校验时），仅清理统计用
  create_time DATETIME    NOT NULL,
  KEY idx_session (session_id),
  KEY idx_user_session (user_id, session_id)
);
CREATE TABLE attachments_deleted (LIKE attachments);  -- 会话删除归档镜像
ALTER TABLE messages ADD COLUMN attachments_json MEDIUMTEXT NULL;
```

- **`attachments` 表 = 文件注册表**：上传时 INSERT；负责所有权校验（`id + user_id + session_id` 三元组匹配）、生命周期删除、配额/孤儿清理预留。
- **`messages.attachments_json` = 链接**：`[{id, filename, mime}]` 数组，随 `model.Message` 整体走现有 write-behind 批量/重试/幂等路径，Redis 消息缓存（整行 JSON）天然携带，历史读取零 join。引用内冗余 filename/mime 是安全的——附件元数据不可变。

**备选（否决）**：仅 `attachments.message_id` 反向链接，不加消息列。链接写入与消息 write-behind 持久化分属两个系统，消息行可见而链接丢失/延迟时历史读到无图消息；join 也拖累 Redis 热路径。`message_id` 回填列保留为 best-effort 统计（失败仅记日志，不进持久化关键路径）。

### D3. 模型渲染：当前轮 inline、历史轮确定性占位符（K=1）

重建 `agent.Message` 时：

- **本轮 user 消息**：文本 part 在前，图片 parts 依次在后（`image_url` data URL，base64 运行时由原文件字节计算——相同字节经 `base64.StdEncoding` 输出确定，轮内多 round 与跨轮窗口内字节级一致，前缀缓存契约成立）
- **历史轮带图消息**：`content` 渲染为原文本 + 换行 + `[图片: {filename}]`（每图一行，按引用顺序）。占位符是确定性函数（无时间戳、无顺序抖动），一经渲染永不变化——每张图片在它所在轮结束后恰好发生一次渲染变化（inline → 占位符），此后所有轮次字节稳定。缓存 miss 的后缀只含占位符文本、不含图片字节；且跨轮缓存本就受 TTL 限制，真正稳定收益在轮内多 round 工具循环（图片 inline 且不变）。
- **compaction 摘要调用 / 子 agent 上下文**：一律使用占位符渲染，图片 parts 不进入。

**备选（否决）**：历史图片永久 inline。前缀字节稳定但每轮重付视觉 token、加速触发 compaction、raw log 与上下文膨胀；"贴图后连续追问"场景暂以 K=1 覆盖，常量可调。

### D4. 管线扩展点：handler 边界做 I/O，agent 包保持纯内存

- `model.Message` 新增 `Attachments []MessageAttachmentRef{ID, Filename, MIME}`（db: attachments_json 反序列化，json 输出）；`agent.Message` 新增 `Images []agent.MessageImage{AttachmentID, MIMEType, Data []byte}`（不进 JSON 序列化，内存态）。
- 历史重建（`MessagesToAgentMessagesIndexed` 路径）在 handler 层判断消息是否属于当前轮：是则读文件填 `Data`，否则渲染占位符。文件读取失败（被外部删除）按占位符降级并记 ERROR 日志——turn 不因缺图中断。
- `openai_client.toOpenAIMessage` 渲染 `OfArrayOfContentParts`：文本 part + 各图片 `image_url` part；无图消息走现有纯文本路径，字节不变。
- 子 agent dispatch（`LastRunMessages`/spawn 上下文构造）与 compaction 输入在各自边界剥掉 `Images`。

**备选（否决）**：复用 `LLMRequest.vision` 侧信道。它单图、附在最后一条 user 消息、请求级而非消息级，语义不匹配；且公共管线需要消息级多图能力。

### D5. 模型能力：catalog `vision` 位，携带附件时严格拒绝

`ModelCatalogEntry` 新增 `Vision bool \`yaml:"vision"\``（缺省 false，无额外校验）。`resolveModelSelection` 结果携带该位；`SendMessage` 在 token 校验后、占 turn 槽前检查：请求带 `attachment_ids` 且解析条目 `vision=false` → 400 `MODEL_NOT_VISION`。`GET /models` 每项新增 `vision` 字段，前端据此禁用/提示。

**备选（否决）**：非 vision 模型自动经 view_image 副驾转述。静默降级损失保真度且引入对识图模型配置的隐式依赖；明确报错让前端引导用户切换模型。

### D6. HTTP 契约：独立上传 + query-token 内容端点

- `POST /api/v1/sessions/:session_id/attachments`（Bearer，multipart `file` 字段）：校验会话所有权 → 魔数嗅探 MIME（仅 `image/png|jpeg|webp|gif`，与 view_image 一致）→ 大小上限（默认 8MiB）→ 落盘 + INSERT 注册表 → 返回 `{attachment_id, filename, mime, size}`。上传即占用 session 附件命名空间，不与消息绑定。
- `POST /messages` 请求体新增可选 `attachment_ids: []string`（≤4）：三元组所有权校验（不属于本用户/本会话 → 404），为空等价于现状。
- `GET /api/v1/sessions/:session_id/attachments/:attachment_id/content?token=<jwt>&inline=1`：挂 `QueryTokenAuthMiddleware`（route-level opt-in，模式同 workspace TokenDownload），校验行归属后 `c.File`。响应头：`ETag: "{attachment_id}"`、`Cache-Control: private, max-age=31536000, immutable`、`Content-Disposition: inline`。附件不可变（重传=新 ID），故比 TokenDownload 的 `no-store` 更激进且安全。
- `GET /sessions/:id/messages` 每条消息新增 `attachments: [{id, filename, mime}]`（来自 attachments_json，无 join）；SSE 用户消息回显事件同步携带。

### D7. raw log 脱敏：图片 part 以元数据替代

llm raw capture 在序列化请求时将每个图片 content part 替换为 `{"type":"image_url_redacted","mime":..., "bytes":..., "sha256":..., "attachment_id":...}` 结构占位，文本 part 原样。结构可解析性不变、"不截断"承诺不破坏（脱敏是有损字段级替换而非长度截断），16MB 上限安全。sha256 与 attachment_id 保留事后排查的回溯能力。

### D8. 生命周期：随会话删除，四步有序

Delete session 流程扩展为：① 同步 drain（现有）→ ② 归档事务中把 `attachments` 行写入 `attachments_deleted` 后删除（同一事务）→ ③ Redis 清理（现有）→ ④ `os.RemoveAll(data/{userID}/attachments/{sessionID})`（目录级幂等，失败仅记日志——行已删，孤儿目录由运营清理）。顺序保证归档完整；孤儿目录不产生正确性问题。

## Risks / Trade-offs

- [占位符降级造成一次性前缀缓存 miss] → 接受：miss 后缀只含文本、有界（每图一次），换取历史不重付视觉 token；K 常量留调空间。
- [附件文件被外部删除/损坏，模型请求缺图] → 重建时降级为占位符 + ERROR 日志，turn 不中断；内容端点返回 404。
- [`?token=` JWT 出现在 URL（日志/历史泄露面）] → 与既有 TokenDownload/artifact content 同等风险面，非新增模式；JWT 有过期时间，泄漏面可接受。
- [上传后未发送的孤儿文件累积] → `message_id` best-effort 回填 + 预留清理任务；不承诺强一致，正确性由消息行引用保证。
- [多图请求超过模型/网关限制] → 每消息上限 4 张 + 单图 8MiB 与 view_image 对齐；网关侧差异由部署方在 catalog 只给确认支持的条目开 `vision`。
- [messages 表膨胀（attachments_json）] → 每条带图消息增量 ~200 字节，远小于 content 本身；可忽略。

## Migration Plan

1. migration 021：`attachments` + `attachments_deleted` + `messages.attachments_json`（存量行为 NULL，语义等同无附件）
2. 代码发布后：存量消息全部无附件 → 渲染路径退化到现有纯文本分支；`vision` 未配置的条目行为不变
3. 回滚：代码回滚即可；新表/新列对旧代码不可见（SELECT 按列名），无需数据回填。已上传文件残留由 attachments 目录随会话删除或运营清理

## Open Questions

- 无（K=1 窗口、每消息 4 张、单图 8MiB、四种 MIME 均为初版常量，实施中如遇网关硬限制再调）
