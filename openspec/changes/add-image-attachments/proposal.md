## Why

用户目前无法在对话中直接让模型查看图片：粘贴截图提问是聊天工作台的高频需求，但现有 `xizhi_view_image` 只是 agent 可主动调用的 workspace 识图副驾，图片不是一等消息附件、不能随用户消息直达主对话管线。需要一条从上传、存储、发送到模型识别的完整链路，同时不破坏现有纯文本管线的前缀缓存契约与持久化分层。

## What Changes

- 新增会话作用域的图片附件上传端点（multipart，MIME 按文件头魔数嗅探，大小/数量有上限），文件落盘到 `data/{userID}/attachments/{sessionID}/{attachmentID}.{ext}`
- 新增 `attachments` 注册表（所有权/配额/生命周期）+ `messages.attachments_json` 引用列表；**base64 永不写入 MySQL/Redis**，发往模型时运行时临时计算
- `POST /messages` 接受可选 `attachment_ids`；每轮解析出的模型目录条目不具备 vision 能力位时返回 400 拒绝（不静默降级）
- 模型目录条目新增可选 `vision` 能力位（缺省 false），`GET /models` 暴露该位
- 发往模型的**当前轮**用户消息运行时展开为 `image_url` base64 content parts（文本 part 在前）；**历史轮**图片渲染为确定性占位符文本，渲染一经产生不再变化
- 新增 query-token 认证的附件内容端点（复用 `QueryTokenAuthMiddleware` 模式，供 `<img>` 等浏览器原生元素使用），附件不可变故响应 immutable 长缓存 + ETag
- 消息历史端点携带结构化 `attachments` 元数据；前端不解析占位符文本（本系统无 SSE 用户消息回显事件——发送期间前端以本地状态渲染，历史读取以 `attachments` 元数据恢复图片）
- 会话删除时归档/删除 `attachments` 行并移除 `attachments/{sessionID}` 目录
- `llm_raw_log` 请求行对图片 content part 脱敏：以 mime/字节数/sha256/attachment_id 元数据替代 base64
- 子 agent 上下文与 compaction 摘要调用不携带图片（文字任务描述/占位符文本）

## Capabilities

### New Capabilities

- `image-attachments`: 用户消息图片附件的端到端能力——上传与校验、session 作用域存储、attachments 注册表与消息引用链接、发送时所有权与模型能力校验、运行时 base64 内联与历史占位符渲染、附件内容端点与缓存、随会话删除的生命周期、raw log 脱敏、子 agent/摘要豁免。

### Modified Capabilities

- `per-request-model-selection`: 模型目录条目新增可选 `vision` 能力位与校验语义；`GET /models` 响应新增该字段；携带附件的请求解析到非 vision 条目时以 400 `MODEL_NOT_VISION` 拒绝。
- `session-crud`: Delete session 的清理范围扩大（附件行归档/删除 + 附件目录移除）；消息列表响应格式新增 `attachments` 数组。
- `llm-raw-capture`: 请求捕获行遇到图片 content part 时 MUST 脱敏存储结构化元数据而非 base64 数据，其余字段不变。

## Impact

- **HTTP 契约**（`api/openapi.yaml` 需同步更新）：新增 `POST /api/v1/sessions/:session_id/attachments`、`GET /api/v1/sessions/:session_id/attachments/:attachment_id/content`（query-token）；`POST /messages` 新增 `attachment_ids`；`GET /sessions/:id/messages` 响应新增 `attachments`；`GET /models` 新增 `vision`。
- **数据库迁移**：新增 `attachments` 表、`attachments_deleted` 归档镜像、`messages.attachments_json` 列。
- **代码面**：`internal/handler`（attachment 新 handler、message_stream 校验与重建、session 删除）、`internal/service`、`internal/store/mysql`（attachments 表 + 归档）、`internal/store/fs`（attachments 目录解析）、`internal/agent`（`Message` 图片 parts、openai_client 渲染、子 agent 剥离）、`internal/config`（catalog vision 位）、llm raw capture 脱敏点。
- **不变量**：现有纯文本会话行为零变化；`vision` 未配置或 false 时目录条目行为与现状一致；附件原文件字节永不改写（无 EXIF 剥离/转码/压缩），保证 base64 与 ETag 确定性。
