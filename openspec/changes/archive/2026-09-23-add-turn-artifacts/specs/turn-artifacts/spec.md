# turn-artifacts Specification

## Purpose

定义 turn 级产物（artifact）能力：agent 每个 turn 对工作空间交付物的产出被可靠检测、按 turn 聚合、通过流事件告知前端，并对产物做版本快照，使历史消息中的文件引用始终能打开"当时的版本"。

## ADDED Requirements

### Requirement: 产物链接语法契约
系统 SHALL 约定 agent 在最终回复中引用交付物时使用 markdown 链接 `[显示名](blowball://workspace/<工作区相对路径>)`（下称产物链接）。产物链接中的路径 SHALL 为工作区相对路径（与 `xizhi_*` 路径规则一致），SHALL NOT 携带版本参数——版本钉定由系统在产物事件中提供，消息文本保持不含版本信息。

#### Scenario: Link form
- **WHEN** agent 在最终回复中引用一个交付物文件
- **THEN** 引用形如 `[季度报告.docx](blowball://workspace/reports/季度报告.docx)`，路径为工作区相对路径且不含查询参数

#### Scenario: Links never point at tmp
- **WHEN** agent 引用任何文件路径
- **THEN** 产物链接 SHALL NOT 指向 `tmp/` 或 `.blowball/` 下的路径

### Requirement: Turn 末产物检测
系统 SHALL 在每个 turn 结束时检测本轮产生的交付物：遍历该用户工作空间，将 mtime 不早于本 turn 开始时间的常规文件记为产物候选。`tmp/`、`.blowball/` 及以 `.` 开头的隐藏目录 SHALL 被排除。空文件与目录 SHALL NOT 记为产物。

#### Scenario: File written by xizhi tool is detected
- **WHEN** 一个 turn 中 agent 通过 xizhi 写工具新建了 `reports/a.docx`
- **THEN** turn 结束时 `reports/a.docx` 被检测为本轮产物

#### Scenario: File written via bash sandbox is detected
- **WHEN** 一个 turn 中 agent 通过 bash 执行脚本在 `/workspace/out/` 生成了文件
- **THEN** turn 结束时这些文件被检测为本轮产物

#### Scenario: tmp and reserved dirs excluded
- **WHEN** 一个 turn 中仅有 `tmp/` 或 `.blowball/` 下的文件被创建或修改
- **THEN** 本轮产物集合为空

#### Scenario: Untouched files are not artifacts
- **WHEN** 一个 turn 未修改某既有文件（mtime 早于 turn 开始）
- **THEN** 该文件不出现在本轮产物集合中

### Requirement: 产物版本快照
系统 SHALL 在 turn 结束时为本轮每个产物创建版本快照：将文件内容上传至 office-vers 服务（`POST /documents/{user_id}/{path}`，位于用户工作空间之外），记录其返回的 `version_id`，并在版本索引中记录 `(user_id, path, version_id, size, mime, created_at)`。同一 `path` 的新快照内容与该 path 最新版本完全一致时 SHALL 复用既有 `version_id` 而不上传（去重在本服务侧完成）。未配置版本服务时 SHALL 降级为仅发产物事件、不携带 version_id。

#### Scenario: Snapshot created for new artifact
- **WHEN** turn 结束且产物 `reports/a.docx` 是首次产出
- **THEN** 文件内容上传至 office-vers，索引新增一条记录携带其返回的 `version_id`

#### Scenario: Overwrite produces new version
- **WHEN** 后续 turn 覆盖了 `reports/a.docx` 且内容发生变化
- **THEN** 索引为该 path 新增一条新版本记录，旧版本内容仍可读取

#### Scenario: Identical content reuses version
- **WHEN** 某 turn 触碰了文件但内容与该 path 最新版本逐字节一致
- **THEN** 该产物复用最新版本的 `version_id`，不写入新快照

#### Scenario: Version store is outside the workspace
- **WHEN** 版本快照被写入
- **THEN** 快照内容存储于 office-vers 服务（不在用户工作空间目录树内），agent 无法通过 `xizhi_*` 或 bash 沙箱读写版本库

#### Scenario: Version store unconfigured degrades gracefully
- **WHEN** `onlyoffice.version_service_url` 未配置且 turn 产出了文件
- **THEN** artifact 事件照常发出但不携带 version_id，不产生快照

### Requirement: artifact 流事件
系统 SHALL 在 turn 末、done 事件之前为本轮每个产物向事件流发出一个 `artifact` 事件，事件 content 为 JSON，包含 `path`、`version_id`、`size`、`mime`、`op`（`create`|`update`）。`artifact` 事件 SHALL 随现有事件持久化管道落库，供历史消息重建时还原产物列表。done 事件的 meta SHALL 附带本轮产物摘要数组（与 artifact 事件同构），该摘要仅用于实时流，不要求持久化。产物集合为空时 SHALL NOT 发出 artifact 事件，done 摘要为空数组。

#### Scenario: Artifact event emitted before done
- **WHEN** 一个 turn 产生了 2 个产物
- **THEN** SSE 流中依次出现 2 个 `artifact` 事件，随后才是 done 事件，且 done 的 meta.artifacts 含 2 个条目

#### Scenario: Artifact events persisted
- **WHEN** turn 完成后客户端重新加载历史消息
- **THEN** 该 turn 的 artifact 事件（含 version_id）可从消息持久化记录中还原

#### Scenario: No artifacts, no events
- **WHEN** 一个 turn 没有产生任何产物
- **THEN** 流中无 artifact 事件，done 的 meta.artifacts 为空数组

### Requirement: 消息重建跳过 artifact 事件
消息重建（`MessagesToAgentMessages`）SHALL NOT 将 artifact 事件作为 token、tool_call 或 tool_result 注入 LLM 上下文；artifact 事件对模型上下文的影响 SHALL 为被跳过或被折叠为不影响对话语义的简短标注。

#### Scenario: Artifact events excluded from LLM context
- **WHEN** 历史消息被重建为 agent 上下文
- **THEN** artifact 事件不产生独立消息，也不拼入 assistant 文本

### Requirement: 版本内容读取接口
系统 SHALL 提供 `GET /api/v1/workspace/versions/{versionId}/content`，返回该版本快照的字节内容与记录的 MIME 类型。接口 SHALL 支持 Bearer 头鉴权与 `?token=` 查询参数鉴权（供浏览器 `<img>`/iframe/OnlyOffice document.url 等无法带头的场景）。版本索引记录与请求用户不匹配时 SHALL 返回 404（不暴露他人版本的存在）。

#### Scenario: Owner reads version content
- **WHEN** 文件属主请求 `GET /api/v1/workspace/versions/{versionId}/content`
- **THEN** 返回快照字节，Content-Type 为索引记录的 mime

#### Scenario: Query token auth
- **WHEN** 请求携带有效 `?token=<jwt>` 而无 Authorization 头
- **THEN** 正常返回版本内容

#### Scenario: Cross-user access denied
- **WHEN** 用户 A 请求属于用户 B 的 versionId
- **THEN** 返回 HTTP 404

#### Scenario: Unknown version
- **WHEN** versionId 不存在
- **THEN** 返回 HTTP 404

### Requirement: 版本解析接口
系统 SHALL 提供 `GET /api/v1/workspace/versions/resolve?path=<rel>`，返回该路径最新版本的 `version_id`；携带 `before=<RFC3339>` 时返回不晚于该时间的最新版本。路径解析越出工作空间时 SHALL 返回 403；该路径从无版本记录时 SHALL 返回 404。

#### Scenario: Resolve latest version
- **WHEN** 请求 `resolve?path=reports/a.docx`
- **THEN** 返回该路径最新 version_id

#### Scenario: Resolve as-of timestamp
- **WHEN** 请求 `resolve?path=reports/a.docx&before=<某 turn 结束时间>`
- **THEN** 返回该时间点之前最近一次快照的 version_id

#### Scenario: Path never versioned
- **WHEN** 请求的路径无任何版本记录
- **THEN** 返回 HTTP 404
