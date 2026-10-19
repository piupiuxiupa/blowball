## 1. 数据层与模型

- [x] 1.1 新增 migration `021_image_attachments.sql`：`attachments` 表、`attachments_deleted` 归档镜像、`messages.attachments_json` MEDIUMTEXT 列
- [x] 1.2 `internal/model`：新增 `Attachment`（attachments 表行）与 `MessageAttachmentRef`（消息内引用）类型；`Message` 增加 `Attachments` 字段（db/json 映射，存量 NULL 等同无附件）
- [x] 1.3 `internal/store/mysql`：attachments 表的 Insert / 按 id+user+session 查询 / 按 session 归档删除（事务内）方法及单元测试
- [x] 1.4 `internal/store/fs`：`UserAttachment(userID, sessionID, attachmentID)` 路径解析与 attachments 目录惰性创建，路径校验拒绝逃逸

## 2. 模型能力位

- [x] 2.1 `internal/config`：`ModelCatalogEntry` 增加 `Vision bool`（yaml `vision`，缺省 false），`resolveModelSelection` 结果携带该位
- [x] 2.2 `internal/handler/model_list.go`：`GET /api/v1/models` 每项输出 `vision` 字段并补测试

## 3. 上传与内容端点

- [ ] 3.1 `internal/handler/attachment.go`：`POST /sessions/:sid/attachments`——会话所有权校验、魔数嗅探（png/jpeg/webp/gif）、8MiB 上限、落盘（attachment_id 命名，filename 不参与路径）、注册表 INSERT
- [ ] 3.2 附件内容端点 `GET /sessions/:sid/attachments/:aid/content`：`QueryTokenAuthMiddleware` 挂载、行归属校验、inline 返回、`ETag` + `Cache-Control: private, max-age=31536000, immutable`、304 支持
- [ ] 3.3 `router.go` 注册两条路由（上传走 Bearer 分区、内容端点走 query-token 分区），`cmd/blowball/serve.go` 完成装配
- [ ] 3.4 handler 单元测试：成功上传、非图片拒绝、超限拒绝、非所有者 404、ETag/304、跨用户 404

## 4. 发送链路

- [ ] 4.1 `sendMessageRequest` 增加 `attachment_ids []string`；解析后先做三元组所有权校验（≤4，非法 404）再做 `vision=false` → 400 `MODEL_NOT_VISION` 拒绝（全部在占 turn 槽与持久化之前）
- [ ] 4.2 用户消息持久化携带 `Attachments` 引用（随现有 write-behind 路径落库），SSE 用户消息回显事件输出 `attachments` 元数据
- [ ] 4.3 发送链路测试：引用他人附件 404、超 4 张 400、非 vision 模型 400 且无副作用、无附件请求行为与现状一致

## 5. 模型管线

- [ ] 5.1 `agent.Message` 增加 `Images []MessageImage`（内存态）；历史重建区分当前轮/历史轮——当前轮读文件填字节，历史轮渲染确定性占位符（读取失败降级 + ERROR 日志）
- [ ] 5.2 `openai_client.toOpenAIMessage`：带图 user 消息渲染 text + image_url content parts（文本在前）；无图消息保持现有纯文本路径字节不变
- [ ] 5.3 子 agent 派发上下文与 compaction 摘要输入剥离 `Images`，统一占位符渲染
- [ ] 5.4 agent 层测试：当前轮 parts 顺序与 base64 一致性、占位符跨轮字节稳定、子 agent/摘要无图

## 6. raw log 脱敏

- [ ] 6.1 llm raw capture 请求序列化对 `image_url` part 替换为 `image_url_redacted`（mime/bytes/sha256/attachment_id），捕获测试断言无 base64 且其余字段原样

## 7. 会话删除生命周期

- [ ] 7.1 Delete session 事务内归档并删除 `attachments` 行（同 `attachments_deleted` 镜像），事务后 `RemoveAll(attachments/{sessionID})`（失败仅记日志）
- [ ] 7.2 删除流程测试：附件随会话归档清理、无附件会话行为不变、目录删除失败不致请求失败

## 8. 契约与端到端验证

- [ ] 8.1 更新 `api/openapi.yaml`：新增两条附件端点、`attachment_ids` 请求字段、消息/SSE 响应 `attachments`、models `vision` 字段
- [ ] 8.2 集成测试：上传 → 携附件发送 → 模型请求含 image part → 下一轮历史为占位符 → 历史列表返回 attachments → 删除会话后附件行/目录消失
- [ ] 8.3 `gofmt` + `go vet ./...` + `make test` 全量通过
