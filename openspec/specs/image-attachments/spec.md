# image-attachments Specification

## Purpose

定义会话作用域的图片附件能力：已鉴权的附件上传端点（会话作用域上传与会话无关的暂存上传，均按文件头魔数嗅探 MIME，仅接受 png/jpeg/webp/gif，单文件 8 MiB 上限）、附件原文件不可变存储与 `attachments` 注册表（`session_id` 可空表示暂存态）、发送消息时的所有权与条数校验（含暂存附件的原子认领绑定）、发往模型的当前轮 content parts 内联与历史轮确定性占位符渲染、子 agent 与摘要上下文不携带图片、暂存孤儿附件的后台清扫、附件内容端点（query token 鉴权与 immutable 长缓存），以及消息读取契约中的附件元数据。

## Requirements

### Requirement: 会话作用域的图片附件上传
系统 SHALL 提供已鉴权的附件上传端点 `POST /api/v1/sessions/:session_id/attachments`（multipart，`file` 字段），仅接受会话所有者的上传。系统 MUST 按文件头魔数（而非扩展名或客户端声明）识别 MIME，且仅接受 `image/png`、`image/jpeg`、`image/webp`、`image/gif`；单文件大小 MUST NOT 超过 8388608 字节（8 MiB）。上传成功 SHALL 返回 `attachment_id`（UUID v7）、原始 `filename`、嗅探后的 `mime` 与 `size`。

#### Scenario: 成功上传
- **WHEN** 会话所有者上传一张 1MB 的 PNG 截图
- **THEN** 系统返回 HTTP 200 与 `{attachment_id, filename, mime: "image/png", size}`，文件落盘到 `data/{userID}/attachments/{session_id}/{attachment_id}.png`，`attachments` 表新增一行

#### Scenario: 非图片格式被拒
- **WHEN** 上传的文件魔数嗅探结果为 `application/pdf`（无论扩展名是什么）
- **THEN** 系统返回 HTTP 400，错误信息列出支持的四种图片格式，不落盘、不写表

#### Scenario: 超过大小上限
- **WHEN** 上传的图片超过 8 MiB
- **THEN** 系统返回 HTTP 413 `FILE_TOO_LARGE`，不落盘、不写表

#### Scenario: 非所有者上传
- **WHEN** 已鉴权用户对不属于自己的会话调用上传端点
- **THEN** 系统返回 HTTP 404，不落盘、不写表

### Requirement: 会话无关的暂存图片上传
系统 SHALL 提供已鉴权的上传端点 `POST /api/v1/attachments`（multipart，`file` 字段），不要求会话存在。MIME 魔数嗅探、格式白名单（`image/png`、`image/jpeg`、`image/webp`、`image/gif`）与 8 MiB 单文件上限 MUST 与会话作用域端点一致。上传成功 SHALL 返回 `{attachment_id, filename, mime, size}`，文件落盘 `data/{userID}/attachments/_staging/{attachment_id}.{ext}`，`attachments` 表新增行且 `session_id` 为 NULL（未绑定暂存态）。系统 SHALL 限制每用户暂存态附件数量（默认 20 个），超限时拒绝上传并返回 HTTP 429。清理失败后的恢复写回 MUST NOT 因配额拒绝：它只重新登记已经存在的文件，同时超限计数 SHALL 阻止后续新上传。

#### Scenario: 无会话成功上传
- **WHEN** 已鉴权用户在不提供任何 `session_id` 的情况下上传一张 1MB 的 PNG
- **THEN** 系统返回 HTTP 200 与 `{attachment_id, filename, mime: "image/png", size}`，文件落盘到 `data/{userID}/attachments/_staging/{attachment_id}.png`，`attachments` 表新增一行且 `session_id` 为 NULL

#### Scenario: 格式与大小约束同会话端点
- **WHEN** 暂存上传的文件嗅探结果为 `application/pdf`，或大小超过 8 MiB
- **THEN** 分别返回 HTTP 400 `UNSUPPORTED_IMAGE_TYPE` 与 HTTP 413 `FILE_TOO_LARGE`，不落盘、不写表

#### Scenario: 暂存配额超限
- **WHEN** 用户已存在 20 个 `session_id` 为 NULL 的暂存附件，再次调用暂存上传
- **THEN** 系统返回 HTTP 429，不落盘、不写表

#### Scenario: 文件删除失败后的恢复不被配额拒绝
- **WHEN** 清扫器已删除过期暂存行但文件删除失败，且并发的暂存上传刚好占满配额
- **THEN** 清扫器仍恢复该暂存行以保留重试入口，随后新的暂存上传返回 HTTP 429

#### Scenario: 暂存附件不提供后端读取
- **WHEN** 以暂存态 `attachment_id` 调用附件内容端点（无论携带哪个会话的 `session_id`）
- **THEN** 系统返回 HTTP 404（composer 预览由前端本地 blob URL 承担）

### Requirement: 附件原文件不可变存储
附件文件字节 SHALL 在上传落盘后永不改写：系统 MUST NOT 对附件做 EXIF 剥离、压缩、转码或任何内容变换；重传同名文件产生新的 `attachment_id` 与新文件。磁盘路径 SHALL 由 `user_id`/`session_id`/`attachment_id` 与嗅探 MIME 对应扩展名拼接，不使用用户提供的 filename 参与路径。

#### Scenario: 相同字节产生相同 base64
- **WHEN** 同一附件在多轮模型请求中被运行时读取并 base64 编码
- **THEN** 每次产生的 data URL 字节级一致

#### Scenario: 用户文件名不参与路径
- **WHEN** 上传的文件名为 `../../evil.png`
- **THEN** 落盘路径仍为 `{attachments}/{session_id}/{attachment_id}.png`，原始文件名仅作为元数据保存与展示

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

### Requirement: 发往模型的图片渲染
历史重建发往 LLM 时，**当前轮**携带附件的用户消息 SHALL 以 content parts 数组发送：文本 part 在前，每个附件一个 `image_url` part 在后，data URL 的 base64 由原文件字节运行时计算。**历史轮**的带图消息 SHALL 渲染为原文本 + 顺序追加的确定性占位符（每附件一行 `[图片: {filename}]`）；占位符渲染一经产生 MUST NOT 随轮次变化。历史轮图片 MUST NOT 以 `image_url` part 发送。附件文件读取失败时 SHALL 降级为占位符并记录 ERROR 日志，turn MUST NOT 因此中断。

#### Scenario: 当前轮消息内联图片
- **WHEN** 本轮用户消息携带 2 个附件且解析模型具备 vision 能力
- **THEN** 该消息以 `[text, image_url, image_url]` content parts 发出，base64 与落盘字节编码一致

#### Scenario: 上一轮图片降级为占位符
- **WHEN** 下一轮发送时重建历史
- **THEN** 上一轮的带图消息以"原文本 + `[图片: {filename}]` 占位符行"的纯文本渲染，不携带任何 image_url part

#### Scenario: 占位符渲染跨轮稳定
- **WHEN** 同一带图历史消息在第 N、N+1、N+2 轮被重建
- **THEN** 其占位符渲染在三轮之间字节级一致

#### Scenario: 附件文件缺失
- **WHEN** 重建时附件文件已被外部删除
- **THEN** 该消息按占位符渲染，系统记录 ERROR 日志，turn 正常继续

### Requirement: 子 agent 与摘要上下文不携带图片
派发子 agent 的上下文消息与 compaction 摘要调用 MUST 使用占位符文本渲染，不携带任何图片 content parts。

#### Scenario: 子 agent 收到文字任务
- **WHEN** 根对话本轮消息带图并触发子 agent 派发
- **THEN** 子 agent 的输入中该消息为"文本 + 占位符"渲染，无 image_url part

#### Scenario: 摘要调用无图
- **WHEN** mid-turn compaction 触发摘要生成
- **THEN** 摘要模型的请求不含任何图片 content part

### Requirement: 附件内容端点与不可变缓存
系统 SHALL 提供 `GET /api/v1/sessions/:session_id/attachments/:attachment_id/content`，经 URL query token（`QueryTokenAuthMiddleware` 语义）鉴权以服务 `<img>` 等浏览器原生元素；校验附件行归属于该用户与会话后以内联方式返回原文件。响应 SHALL 携带 `ETag: "{attachment_id}"` 与 `Cache-Control: private, max-age=31536000, immutable`。附件不存在或不属于该用户/会话时返回 404。

#### Scenario: query token 渲染图片
- **WHEN** 前端以 `<img src=".../attachments/{id}/content?token=<jwt>&inline=1">` 请求
- **THEN** 系统返回 HTTP 200、嗅探 MIME 的 Content-Type、immutable 长缓存头与原文件字节

#### Scenario: 条件请求命中缓存
- **WHEN** 浏览器携带 `If-None-Match: "{attachment_id}"` 再次请求
- **THEN** 系统返回 HTTP 304，不重传文件体

#### Scenario: 跨用户读取被拒
- **WHEN** 用户 A 以有效 JWT 请求用户 B 会话的附件内容
- **THEN** 系统返回 HTTP 404

### Requirement: 消息读取契约携带附件元数据
`GET /api/v1/sessions/:session_id/messages` 的每条消息 SHALL 在携带附件时包含 `attachments: [{id, filename, mime}]` 数组（来自消息行引用，无表 join）。无附件消息 SHALL 省略该字段，与变更前响应兼容。本系统不产生 SSE 用户消息回显事件：发送期间前端以本地粘贴状态渲染，重放/刷新后凭本端点的 `attachments` 元数据恢复图片渲染。

#### Scenario: 历史列表返回附件元数据
- **WHEN** 会话包含带图用户消息且客户端请求消息列表
- **THEN** 对应消息项含 `attachments` 数组，客户端可据 `id` 拼接内容端点 URL 渲染图片

#### Scenario: 无附件消息响应兼容

- **WHEN** 消息列表包含不带附件的历史消息
- **THEN** 这些消息项不含 `attachments` 字段，响应形状与变更前一致
