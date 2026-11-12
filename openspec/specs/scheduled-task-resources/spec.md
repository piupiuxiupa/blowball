# scheduled-task-resources Specification

## Purpose

Define user-owned .cron task resources, optional published-version bindings, fenced immutable publication, bounded safe access, pinned execution, mutation protection, independent retention, and the shared workspace data plane.

## Requirements

### Requirement: User-owned cron resource namespace

任务资源 SHALL 位于用户工作空间 .cron/<task_id>/，包含可编辑 draft、受保护发布 revisions 和受控 staging/元数据。根路径 SHALL 由服务端按 user/task 身份解析，客户端 SHALL NOT 指定宿主绝对路径或他人 task。未受管理的既有目录冲突 SHALL 返回明确错误，不自动覆盖或清空。

#### Scenario: Prepare resources for an owned task
- **WHEN** 用户为自己任务准备脚本、文档和附件
- **THEN** 资源写入该任务 draft，不进入其他用户或普通会话附件目录

#### Scenario: Existing unmanaged cron directory
- **WHEN** 初始化目标命名空间发现冲突的非受管理内容
- **THEN** 返回 RESOURCE_NAMESPACE_CONFLICT，原目录和文件保持不变

### Requirement: Optional resources and explicit configuration binding

资源 SHALL 为可选任务配置。带文件任务 SHALL 先获得所属 Task 身份，在 draft 编辑、发布后显式绑定 resource_revision/manifest_hash；无文件任务 SHALL 可以使用空引用。配置引用 SHALL 只接受已发布、属于该用户/task 的版本，SHALL NOT 接受 draft/latest 路径。

#### Scenario: Create a prompt-only task
- **WHEN** 用户创建不需要附件的合法任务
- **THEN** 可以启用并执行，资源引用为空，不要求创建占位资源包

#### Scenario: Publication succeeds but task update conflicts
- **WHEN** 发布成功但 Task expected_revision 过期或任务已删除
- **THEN** 不替换 Task 当前引用，不使半成品任务可执行，发布版本可按幂等身份查询

### Requirement: Fenced immutable resource publication

主服务 SHALL 串行化同一任务的受控草稿写入与发布，使用可跨 API 节点的写入围栏和 draft revision。发布 SHALL 校验普通文件/相对路径、复制到受保护 staging、对实际复制字节生成按路径排序的路径/大小/SHA-256 manifest，并确认 generation 后原子发布和保存资源版本元数据。相同 publish request ID SHALL 幂等；已发布版本 SHALL 不可原地改变。

#### Scenario: Concurrent edit races with publish
- **WHEN** 草稿更新与发布并发，或调用者 draft revision 已过期
- **THEN** 发布获得一致的受控版本或返回明确冲突，不把变化中的 draft 标记为既有不可变版本

#### Scenario: Publisher crashes before visibility
- **WHEN** 发布者在 staging 或元数据确认前崩溃
- **THEN** 任务不能引用半成品；恢复可查询/完成同一发布或清理未引用 staging，不覆盖既有版本

#### Scenario: Retry the same publication
- **WHEN** 发布响应丢失后以相同 request ID 查询/重试
- **THEN** 返回同一 resource_revision 和 manifest_hash，不产生不同内容的同一版本

### Requirement: Bounded publication and safe path access

发布 SHALL 有正值文件数量/总字节限制并遵循现有单文件上传限制。绝对路径、路径穿越、符号链接逃逸、设备、FIFO 和超限输入 SHALL 被拒绝；保护 SHALL 覆盖检查后替换的竞态及祖先/移动目标，不只进行字符串前缀检查。失败 SHALL 不改变已有绑定版本。

#### Scenario: Reject unsafe resource entries
- **WHEN** draft 包含逃逸路径、符号链接、非普通文件或超过配置上限
- **THEN** 发布失败，当前可执行版本不变，不读取或写入工作空间外资源

### Requirement: Pinned execution resources

每个 occurrence SHALL 固定 resource_revision 和 manifest_hash。Runtime SHALL 在启动前验证 owner、manifest 和文件内容，并提供有界的资源上下文供 Agent 按需读取。资源内容 SHALL 不替换系统安全策略，Task.prompt SHALL 以原文持久化；SHALL NOT 自动把全部附件注入模型或自动执行目录中的脚本。

#### Scenario: A newer version is published during a pending run
- **WHEN** 新资源发布并绑定 Task，而旧 occurrence 已物化
- **THEN** 旧执行仍读取旧 pinned version，新物化触发使用新引用

#### Scenario: A pinned version is missing or corrupted
- **WHEN** 启动前资源缺失、摘要不符或归属非法
- **THEN** 不执行 Agent，不降级读取 draft/latest，记录明确失败并按未启动恢复规则处理

### Requirement: Protected mutation across all execution paths

用户与 Agent SHALL 仅能经授权入口修改 draft。发布版本、staging、管理元数据及可覆盖它们的祖先/移动目标 SHALL 不能经通用 REST、文件工具、Office 回调或 bash 修改。bash SHALL 对 .cron 整体只读；草稿生成 SHALL 使用受控文件工具/API。合法资源读取及其他工作空间行为保持。

#### Scenario: Attempt to remove an ancestor
- **WHEN** 用户或 Agent 尝试删除/移动 .cron 或任务根以间接破坏 revisions
- **THEN** 操作被拒绝，发布版本和正在执行的资源保持

#### Scenario: Bash attempts to change a task resource
- **WHEN** 普通或定时会话的 bash 写入 .cron/draft 或 revisions
- **THEN** 被只读沙箱挂载拒绝；受控文件入口仍可修改授权 draft

#### Scenario: A controlled file tool creates replaces or appends a draft file
- **WHEN** owner 经文件工具写入自己的合法 draft，使用默认、write 或 append 模式
- **THEN** 传递受支持的 guard 操作类型，先检查 owner 并推进 draft fence，再执行文件修改
- **AND** 发布区、他人草稿及 fence 不可用时仍在修改文件前拒绝

### Requirement: Resource lifecycle independent of session deletion

删除/暂停 Task SHALL 不立即清理执行引用资源；删除单个任务会话 SHALL 不删除 .cron 或解除保留历史的资源引用。Task、保留 occurrence 或运行中的执行引用的版本 SHALL 保留；受控维护仅能清理已证明未引用且无活跃使用的 staging/版本。

#### Scenario: Delete a session whose execution used a resource version
- **WHEN** 用户删除已完成任务会话，但触发历史仍保留
- **THEN** 该历史的资源版本仍保留，任务可继续按当前配置触发

#### Scenario: Task is deleted while a Run is active
- **WHEN** Task 软删除且已有 Run 使用发布版本
- **THEN** 不清理该版本，已启动 Run 仍能读取固定资源并保存终态

### Requirement: Resources share the existing workspace data plane

资源文件 SHALL 由主服务工作空间能力管理；多 API/Agent 节点 SHALL 遵循现有共享 POSIX 存储、用户作用域及 bwrap 可访问性检查。Scheduler SHALL 不挂载工作空间，不增加本地资源副本作为事实来源。

#### Scenario: Publish and execute on different nodes
- **WHEN** API 节点发布资源而另一 Agent 节点执行任务
- **THEN** Agent 在已配置共享工作空间读取同一 pinned version；挂载不满足既有要求时不静默转本地文件
