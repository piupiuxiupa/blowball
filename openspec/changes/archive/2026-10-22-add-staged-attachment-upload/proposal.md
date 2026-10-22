# add-staged-attachment-upload

## Why

当前图片附件上传端点 `POST /sessions/:session_id/attachments` 要求会话已存在且归属本人，磁盘目录也按 `data/{user}/attachments/{session}/` 组织。这导致**新会话场景下图片无法在首条消息之前上传**：客户端必须先建会话、再串行传完全部图片、最后才能发消息——弱网环境下用户在点"发送"后要长时间空等。上传需要与会话解耦：图片随选随传进入暂存区，发送时后端按 id 认领并绑定到会话。

## What Changes

- 新增会话无关的上传端点 `POST /api/v1/attachments`（Bearer，multipart `file` 字段）：嗅探/大小/格式约束与现有端点完全一致，文件落盘到每用户暂存目录 `data/{userID}/attachments/_staging/`，注册表行 `session_id` 为 NULL（未绑定）。
- `attachments.session_id` 改为可空（migration 022）：NULL 表示"已上传、未绑定"的暂存态；存量行不受影响。
- 发送消息时 `attachment_ids` 的所有权校验从"`id + user_id + session_id` 三元组相等"放宽为"`id + user_id` 匹配且 `session_id IS NULL OR = 本会话`"；对暂存态附件执行**原子认领**（条件 UPDATE，抢到才继续）并将文件从暂存目录 rename 进会话目录。认领失败（他会话并发抢占）返回 404，语义与"引用他人附件"一致。同一附件一旦被某会话绑定，MUST NOT 再被其他会话引用。
- 新增暂存孤儿清扫：后台定时任务清理"上传后超过 24h 仍未绑定"的暂存附件（删行 + 删文件），并对每用户暂存附件数量设上限（默认 20 个，超出拒绝上传，HTTP 429）。
- 现有 `POST /sessions/:session_id/attachments` 保留为"上传即绑定"的兼容别名，行为不变。
- `api/openapi.yaml` 同步新增/更新契约。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `image-attachments`: 上传端点从会话作用域扩展为"会话无关暂存 + 发送时认领绑定"；新增暂存态生命周期（认领、孤儿 TTL 清扫、暂存配额）；发送时附件校验语义放宽为接受本会话已绑定或未绑定附件。注意：`image-attachments` 能力的主 spec 由已完成但未归档的 `add-image-attachments` 变更承载，本变更应在其归档/同步之后落地归档。

## Impact

- **代码**：`internal/handler/attachment.go`（新暂存上传 handler、配额检查）、`internal/handler/message_stream.go`（校验放宽 + 认领 + rename）、`internal/store/mysql/attachment.go`（新查询：条件认领 UPDATE、暂存配额计数、TTL 孤儿扫描/删除）、`internal/store/fs/user.go`（暂存目录解析）、`internal/service/session.go` 或 `internal/run/`（后台清扫任务，模式参照 drain keeper）、`internal/handler/router.go`（新路由）。
- **API**：新增 `POST /api/v1/attachments`；`POST /sessions/:id/messages` 的 `attachment_ids` 接受暂存 id；`api/openapi.yaml` 更新。
- **数据库**：migration 022（`attachments.session_id` 改 NULLABLE + 清扫用索引）。现有数据库需手动执行该 migration（Docker initdb 仅首次初始化生效）。
- **兼容性**：读取面零变化——内容端点、历史渲染、占位符降级、删会话 `RemoveAll` 均保持现状（rename 后路径形态不变）；旧上传端点保留。客户端可平滑迁移到"选中即传"。
- **依赖顺序**：`add-image-attachments` 已完成（23/23）但尚未归档；本变更的 spec delta 以其为基线，归档本变更前需先归档/同步它。
