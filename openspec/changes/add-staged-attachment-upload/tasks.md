## 1. Schema 与存储层

- [x] 1.1 新建 `migrations/022_staged_attachments.sql`：`attachments.session_id` 改为 NULLABLE，新增 `KEY idx_session_created (session_id, create_time)`，`attachments_deleted` 镜像列同步可空
- [x] 1.2 `internal/store/fs/user.go`：新增 `UserAttachmentStagingDir(userID)`（`{root}/{userID}/attachments/_staging`），补 `fs_test.go` 用例
- [x] 1.3 `internal/store/mysql/attachment.go`：新增 `GetAttachmentByUser(ctx, id, userID)`（不带 session 的归属查询）、`ClaimAttachment(ctx, id, userID, sessionID) (bool, error)`（条件 UPDATE，affected rows 判成功）、`UnclaimAttachment(ctx, id, userID)`（best-effort 回滚为 NULL）、`CountStagedAttachments(ctx, userID)`、`ListExpiredStagedAttachments(ctx, cutoff)`、`DeleteAttachment(ctx, id)`；补单测

## 2. 暂存上传端点

- [x] 2.1 `internal/handler/attachment.go`：抽取上传核心（魔数嗅探/大小上限/写盘/插行）为共享辅助函数，供新旧两端点复用；新增 `UploadStaged` handler——配额 COUNT 检查（20 个，超限 429）→ 写 `_staging` 目录 → 插入 `session_id` 为 NULL 的行
- [x] 2.2 `internal/handler/router.go`：注册 `POST /api/v1/attachments`（Bearer）；旧端点保持原路由原行为（上传即绑定）
- [x] 2.3 `internal/handler/attachment_test.go`：暂存上传成功/格式拒绝/超限 413/配额 429/会话归属不影响暂存上传 等用例

## 3. 发送时认领与绑定

- [ ] 3.1 `internal/handler/message_stream.go`：附件校验改为 `GetAttachmentByUser`；按行状态分流（本会话已绑定→放行 / 已绑定他人→404 / 暂存态→`ClaimAttachment` 认领，失败 404）
- [ ] 3.2 认领成功后 `os.MkdirAll` 会话目录 + `os.Rename` 暂存文件；rename 失败时 `UnclaimAttachment` best-effort 回滚 + ERROR 日志 + 该附件走既有缺失降级（占位符），turn 不中断
- [ ] 3.3 `internal/handler/message_stream_persist_test.go` / `turn_run_test.go`：认领成功、并发抢占仅一方成功（可用串行模拟两次认领）、同会话重复引用幂等、跨会话引用已绑定附件 404、rename 失败降级 等用例

## 4. 孤儿清扫后台任务

- [ ] 4.1 新增清扫组件（参照 `internal/run/drain.go` keeper 模式）：ticker 每 10 分钟执行一轮，删除 `session_id IS NULL AND create_time < now()-24h` 的行与对应 `_staging` 文件；单行失败记日志继续；服务启动时注册、关停时退出
- [ ] 4.2 清扫单测：过期删除、未过期跳过、已绑定不触碰、文件缺失时删行继续

## 5. 契约与验证

- [ ] 5.1 `api/openapi.yaml`：新增 `POST /api/v1/attachments`（响应/错误码），更新 `attachment_ids` 描述为接受本会话已绑定或暂存附件
- [ ] 5.2 `test/integration/`：端到端——暂存上传 → 新会话首条消息认领绑定 → 历史读取附件元数据 → 内容端点可读；删会话不触碰其他暂存附件
- [ ] 5.3 `make test`（race）与 `make lint` 全绿；确认 `loadCurrentTurnImages`、内容端点、删会话 `RemoveAll` 路径未被改动
- [ ] 5.4 归档前确认 `add-image-attachments` 已归档/同步（本变更 spec delta 的基线）
