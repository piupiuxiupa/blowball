# scheduled-task-sessions Specification

## Purpose

Define physically isolated scheduled conversation storage, stable occurrence sessions, source-bound persistence and caches, canonical runtime behavior, read-only history, immutable snapshots, independent lifecycles, and legacy provenance.

## Requirements

### Requirement: Physically separate scheduled conversation storage

系统 SHALL 为任务会话建立独立 scheduled_sessions 及消息、工具记录、标题、用量、压缩、子 Agent 和所需附件/归档表组，关联外键 SHALL 指向任务会话主表。系统 SHALL NOT 在普通 sessions 创建占位行或将新任务对话内容写入普通消息表以兼容旧存储。

#### Scenario: Execute a task with tools and a sub-agent
- **WHEN** 新任务产生主 Agent 消息、工具调用及子 Agent transcript
- **THEN** 数据写入相应 scheduled 表，普通 sessions/messages/tool_calls 无该任务数据
- **AND** 子 Agent 内容保持在独立 transcript 中，不回流任务主消息表

#### Scenario: Persist compaction and usage
- **WHEN** 任务发生上下文压缩并保存用量
- **THEN** 压缩标志、记录和上下文计量均使用 scheduled 表，不更新普通 sessions

### Requirement: One stable session per occurrence

每个新 occurrence SHALL 对应一个归属任务所有者的新任务会话，session ID SHALL 在准备前固定，occurrence_id SHALL 唯一。重复提交/恢复同一 occurrence SHALL 复用同一任务会话；不同触发 SHALL NOT 追加到同一会话或普通用户会话。

#### Scenario: Consecutive cron occurrences
- **WHEN** 同一任务先后物化两个触发
- **THEN** 两个触发具有不同 scheduled session，重复准备各自触发不新增会话

### Requirement: Source-bound persistence and cache access

Runtime SHALL 复用现有 turn 引擎，以服务端验证的会话来源选择固定存储适配。适配 SHALL 覆盖 send-time、round 双表事务、终局、标题、压缩、子 Agent、附件、缓存回填及删除。读取缓存前 SHALL 在正确的主表验证 owner/source，SHALL NOT 用普通表查无记录后的跨表 fallback 推断来源。

#### Scenario: A scheduled ID is used in an ordinary endpoint
- **WHEN** 用户把任务 session ID 提交给普通会话读取、发送消息或删除接口
- **THEN** 返回 404，不读写任务会话，即使用户也是任务所有者

#### Scenario: Cache miss rebuilds a task session
- **WHEN** scheduled session 缓存缺失
- **THEN** 只从对应 scheduled 表组按现有 canonical 规则重建，不写普通缓存内容或普通数据库表

#### Scenario: Production headless execution uses scheduled services
- **WHEN** Agent/all 接收权威来源为 scheduled 的执行
- **THEN** 共用 turn 入口同时使用 scheduled sessions/messages/titles/compaction/sub-agent snapshots 服务
- **AND** 没有配置该表组时继续 fail-closed，不回退普通服务

#### Scenario: Headless title generation preserves owner credentials
- **WHEN** Runtime 使用没有 HTTP 用户身份的后台 context 提交任务，且 owner 已配置专属 LLM token
- **THEN** 共用 turn 入口在标题及模型调用之前从已验证的 Request.UserID 绑定用户上下文
- **AND** 标题与主 turn 按同一 owner 解析凭证，不因后台入口绕过用户 token 使用全局凭证

### Requirement: Preserve canonical runtime behavior

任务会话 SHALL 遵守现行 round 终稿事务、工具顺序、子 Agent 分流、压缩锚点和用户原文规则，不复活全局 chunk 入库队列。current_status/tool_budget SHALL 继续只存在于 active transcript 和独立 runtime cache；SHALL NOT 写入任何 MySQL/消息缓存、SSE、标题、真实用户序号或子 Agent transcript。任务的跨会话记忆 SHALL 继承现有开关、授权和读写策略，不因分表另行改变。

#### Scenario: Budget guidance appears during task execution
- **WHEN** Runtime 注入状态或工具预算倒计时
- **THEN** 任务数据库只保存真实任务指令/对话，cache-only guidance 不产生消息行

#### Scenario: Memory behavior survives storage separation
- **WHEN** 任务调用既有跨会话记忆能力
- **THEN** 按明确任务来源读取历史并遵守原开关/授权/策略，不将状态或资源元数据当作用户原文

### Requirement: Dedicated read-only task session interfaces

API/all SHALL 提供已鉴权的 scheduled-sessions 列表、详情、消息游标读取和删除接口；普通列表 SHALL 不包含新任务会话。首期 SHALL NOT 提供任务会话自由发送消息或手动重跑。列表 SHALL 默认 limit 20、最大 100；消息游标/order/subagent_content SHALL 沿用现有读取语义，跨用户资源 SHALL 返回 404。

#### Scenario: Display scheduled history separately
- **WHEN** 用户读取普通会话列表和任务会话列表
- **THEN** 两者数据来源独立，任务列表可按 task_id 查询，普通列表无新任务会话

#### Scenario: Read another user's task session
- **WHEN** 用户请求他人任务会话详情、消息或删除
- **THEN** 返回 404，且不读取缓存、排空、修改或删除该资源

#### Scenario: Reject conversational follow-up
- **WHEN** 客户端尝试向任务会话发送用户消息
- **THEN** 没有可用的发送消息接口，不创建新的交互 Run

#### Scenario: Read scheduled placeholder pages
- **WHEN** owner 使用 subagent_content=placeholder 按升序或降序读取任务消息，或请求空页
- **THEN** 在 scheduled 表组内先过滤再分页，不因普通表限定名产生 SQL 错误

#### Scenario: Read back a persisted scheduled sub-agent run
- **WHEN** 任务子 Agent 已保存 run，随后请求 list/get/child/history
- **THEN** 均能从 scheduled 表组读回同一 run 及完整时间字段，包括增量迁移之前已写入的行

### Requirement: Immutable historical configuration and editable parent task

任务会话详情 SHALL 返回 task_id、occurrence/run 身份、该次 immutable 配置/资源快照及实际模型。当前可编辑配置 SHALL 通过所属任务管理接口获得，SHALL NOT 覆盖历史快照。编辑当前 Task SHALL 不修改已物化触发、历史内容或正在使用的资源版本。

#### Scenario: Edit from an old execution
- **WHEN** 用户从历史任务会话修改所属任务的指令、模型或时间
- **THEN** 当前 Task 更新，旧会话仍显示原快照与实际模型，后续新触发使用新配置

### Requirement: Independent cancellation deletion and archive

Agent/all SHALL 提供仅所有者可用的任务 Run 显式取消入口，使用既有取消/部分持久化机制。运行中或尚未证明停止（包括 uncertain/hold）的任务会话删除 SHALL 返回 409；已停止会话删除 SHALL 按归档规则处理 scheduled 表组并清理该会话缓存，不删除 Task/Occurrence 或 .cron。用户删除 SHALL 先处理任务会话归档，避免外键级联造成未归档丢失。

#### Scenario: Delete one completed session
- **WHEN** 用户删除已结束的任务会话
- **THEN** 只归档/清理该 scheduled session 内容，任务、触发事实和资源仍保留

#### Scenario: Delete while execution is active
- **WHEN** 用户删除运行中或历史 interrupted 但仍可能执行的任务会话
- **THEN** 返回 409；取得可靠停止证明并完成收尾后才能删除

### Requirement: Explicit legacy history provenance

遗留 occurrence SHALL 显式标记 legacy_interactive 来源，新 occurrence SHALL 标记 scheduled。历史读取 SHALL 根据权威来源路由，不自动搬迁、隐藏或重分类遗留普通会话。历史内容已删除时 SHALL 保留触发事实和内容不可用信息。

#### Scenario: Legacy task reused a real chat
- **WHEN** 遗留触发指向用户普通聊天
- **THEN** 该普通会话保持原表和原归属，任务历史按明确 legacy 来源读取，不生成 scheduled 占位副本
