## MODIFIED Requirements

### Requirement: Bash command execution tool

系统 SHALL 保留名为 bash 的工具，在既有 bubblewrap 沙箱及用户工作空间中执行命令。既有系统/技能/操作员挂载规则 SHALL 保持；用户 .cron SHALL 在所有普通和任务 bash 中只读，不能经 shell 改写草稿或发布区。任务草稿生成 SHALL 使用受控文件工具/API，不新增 Firecracker、风险审查或网络策略。

#### Scenario: Successful bash command
- **WHEN** Agent 调用 bash 执行 echo hello
- **THEN** 在 bwrap 的 /workspace 工作目录运行，返回合并 stdout/stderr 与 exit code
- **AND** 全局技能保持 /skills/global 只读，用户技能按既有 workspace/.blowball/skills 布局可达，操作员/market 挂载行为不变

#### Scenario: Bash command timeout
- **WHEN** 命令超过工具 timeout
- **THEN** 终止沙箱进程，返回现有超时错误

#### Scenario: Bash command output limit
- **WHEN** 合并输出超 max_output_bytes
- **THEN** 按既有上限和 marker 截断

#### Scenario: Bash command outside workspace and skill directories access denied
- **WHEN** 命令访问现有声明挂载根之外的未授权路径
- **THEN** bwrap 拒绝访问并返回命令错误，不新增可见宿主目录

#### Scenario: Bash cannot write the cron namespace
- **WHEN** 普通或任务 bash 尝试写 .cron 的 draft/revisions、改名或删除该命名空间
- **THEN** 只读挂载拒绝操作，其他合法 workspace 写入保持

#### Scenario: Cron namespace is initially absent
- **WHEN** 用户工作空间尚未初始化 .cron
- **THEN** 沙箱呈现只读空命名空间或在受控初始化后只读绑定，不允许由 bash 创建可写 .cron 绕过后续保护
