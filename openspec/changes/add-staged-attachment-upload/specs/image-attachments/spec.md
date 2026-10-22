## ADDED Requirements

### Requirement: 会话无关的暂存图片上传
系统 SHALL 提供已鉴权的上传端点 `POST /api/v1/attachments`（multipart，`file` 字段），不要求会话存在。MIME 魔数嗅探、格式白名单（`image/png`、`image/jpeg`、`image/webp`、`image/gif`）与 8 MiB 单文件上限 MUST 与会话作用域端点一致。上传成功 SHALL 返回 `{attachment_id, filename, mime, size}`，文件落盘 `data/{userID}/attachments/_staging/{attachment_id}.{ext}`，`attachments` 表新增行且 `session_id` 为 NULL（未绑定暂存态）。系统 SHALL 限制每用户暂存态附件数量（默认 20 个），超限时拒绝上传并返回 HTTP 429。

#### Scenario: 无会话成功上传
- **WHEN** 已鉴权用户在不提供任何 `session_id` 的情况下上传一张 1MB 的 PNG
- **THEN** 系统返回 HTTP 200 与 `{attachment_id, filename, mime: "image/png", size}`，文件落盘到 `data/{userID}/attachments/_staging/{attachment_id}.png`，`attachments` 表新增一行且 `session_id` 为 NULL

#### Scenario: 格式与大小约束同会话端点
- **WHEN** 暂存上传的文件嗅探结果为 `application/pdf`，或大小超过 8 MiB
- **THEN** 分别返回 HTTP 400 `UNSUPPORTED_IMAGE_TYPE` 与 HTTP 413 `FILE_TOO_LARGE`，不落盘、不写表

#### Scenario: 暂存配额超限
- **WHEN** 用户已存在 20 个 `session_id` 为 NULL 的暂存附件，再次调用暂存上传
- **THEN** 系统返回 HTTP 429，不落盘、不写表

#### Scenario: 暂存附件不提供后端读取
- **WHEN** 以暂存态 `attachment_id` 调用附件内容端点（无论携带哪个会话的 `session_id`）
- **THEN** 系统返回 HTTP 404（composer 预览由前端本地 blob URL 承担）

### Requirement: 发送时的暂存附件认领与绑定
发送消息携带暂存态 `attachment_id` 时，系统 SHALL 在占 turn 槽之前执行原子认领：以条件 UPDATE（`id + user_id` 匹配且 `session_id IS NULL`）绑定到本会话，成功后将文件从暂存目录 rename 进本会话附件目录。认领失败的 id SHALL 按 HTTP 404 处理。附件一旦绑定到某会话，MUST NOT 被其他会话引用；同一会话重复引用已绑定附件 SHALL 放行且不重复 rename。rename 失败时系统 SHALL best-effort 将认领回滚（恢复 `session_id` 为 NULL）、记录 ERROR 日志，并将该附件按缺失文件路径降级为占位符，turn MUST NOT 因此中断。

#### Scenario: 新会话首条消息认领成功
- **WHEN** 新会话的首条消息携带 2 个暂存态 `attachment_ids`
- **THEN** 两行的 `session_id` 绑定为本会话，文件 rename 至本会话附件目录，当前轮图片正常 inline 给模型

#### Scenario: 并发抢占仅一方成功
- **WHEN** 两个不同会话的发送请求同时引用同一暂存 `attachment_id`
- **THEN** 仅一个请求认领成功并继续；另一个收到 HTTP 404，不占 turn 槽、不产生持久化

#### Scenario: 同会话引用已绑定附件幂等放行
- **WHEN** 同一会话的后续消息（含 client_msg_id 重试）引用已绑定到本会话的附件
- **THEN** 校验通过、不发生 rename，行为与既有已绑定附件一致

#### Scenario: 跨会话引用已绑定附件被拒
- **WHEN** 会话 B 的消息引用已绑定到会话 A 的附件
- **THEN** 系统返回 HTTP 404，不占 turn 槽、不产生持久化

#### Scenario: rename 失败降级
- **WHEN** 认领 UPDATE 成功但文件 rename 失败
- **THEN** 系统 best-effort 将行恢复为未绑定、记录 ERROR 日志，该附件以占位符渲染进入模型请求，turn 正常继续

### Requirement: 暂存附件的孤儿清扫
系统 SHALL 运行后台清扫任务，删除上传超过 24 小时仍未绑定（`session_id IS NULL`）的暂存附件：删除注册表行并删除对应暂存文件。清扫 MUST 为 best-effort——单行处理失败仅记录日志，后续周期重试；已绑定附件（`session_id` 非 NULL）MUST NOT 被该任务触碰。

#### Scenario: 过期暂存附件被回收
- **WHEN** 暂存附件的 `create_time` 距今超过 24 小时
- **THEN** 下一轮清扫删除其注册表行与 `_staging` 文件

#### Scenario: 未过期暂存附件保留
- **WHEN** 暂存附件上传不足 24 小时
- **THEN** 清扫任务跳过该行与对应文件

#### Scenario: 已绑定附件不受清扫影响
- **WHEN** 附件已绑定到会话（`session_id` 非 NULL）且上传超过 24 小时
- **THEN** 清扫任务不处理该附件（其生命周期归删会话流程）

#### Scenario: 文件已缺失时删行继续
- **WHEN** 某过期暂存行的文件已被外部删除
- **THEN** 系统删除该行并继续处理其余行，任务不中断

## MODIFIED Requirements

### Requirement: 附件注册表与消息引用
系统 SHALL 以 `attachments` 表作为附件注册表（`id`、`user_id`、`session_id`、`filename`、`mime`、`size`、`create_time`），其中 `session_id` 可空——NULL 表示"已上传、未绑定"的暂存态（见"会话无关的暂存图片上传"），非 NULL 表示已绑定会话；以 `messages.attachments_json` 引用数组（`[{id, filename, mime}]`）承载消息与附件的链接。引用 MUST 随消息行经现有 write-behind 持久化路径原子落库；系统 MUST NOT 在 MySQL、Redis 或任何持久层存储 base64 图片内容。

#### Scenario: 引用随消息幂等持久化
- **WHEN** 携带附件的用户消息经 write-behind 队列重试后最终落库
- **THEN** 消息行的 `attachments_json` 与消息内容同批次写入，重投递不产生重复引用

#### Scenario: Redis 缓存携带引用
- **WHEN** 消息列表从 Redis 缓存命中返回
- **THEN** 带图消息的缓存 JSON 内含 attachments 引用数组，无需回表

### Requirement: 发送消息时的附件校验
`POST /messages` SHALL 接受可选 `attachment_ids` 数组（每条消息最多 4 个）。每个 id MUST 满足：`id + user_id` 匹配，且其 `session_id` 等于本会话或为 NULL（暂存态）；暂存态附件 SHALL 按"发送时的暂存附件认领与绑定"完成认领后方可使用，认领失败按 HTTP 404 处理。不属于本用户的 id、或已绑定到其他会话的 id，SHALL 返回 HTTP 404。数组为空或缺失时请求行为与现有纯文本消息完全一致。携带附件时，模型能力校验 SHALL 按 per-request-model-selection 的 vision 能力位执行。

#### Scenario: 引用他人附件被拒
- **WHEN** 请求携带的 `attachment_ids` 中存在属于其他用户、或已绑定到其他会话的 id
- **THEN** 系统返回 HTTP 404，不占 turn 槽、不产生任何持久化

#### Scenario: 引用暂存附件经认领放行
- **WHEN** 请求携带的 `attachment_ids` 为本用户的暂存态附件
- **THEN** 认领成功后校验通过，turn 正常开始

#### Scenario: 超过单消息附件数上限
- **WHEN** 请求携带 5 个有效 `attachment_ids`
- **THEN** 系统返回 HTTP 400，错误信息指明上限为 4

#### Scenario: 无附件请求零变化
- **WHEN** 请求不携带 `attachment_ids`（或为空数组）
- **THEN** 校验、持久化、模型请求路径与变更前的纯文本行为字节一致
