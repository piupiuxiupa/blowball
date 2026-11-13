# scheduled-task-service Delta

## MODIFIED Requirements

### Requirement: Independent scheduler in a monorepo

系统 SHALL 在同一 Git 仓库的 `services/scheduler/` 提供独立 Scheduler 启动、配置、构建和部署入口。monorepo 终态下,根 Go module SHALL 永久作为全部服务的单一 module,主服务入口 SHALL 按服务化布局提供(`blowball-api`/`blowball-agent` 独立二进制与根 `cmd/blowball` 开发单体并存)。Scheduler 二进制 SHALL 规范化命名为 `blowball-scheduler`,其部署工件 SHALL 位于 `deploy/scheduler/`。Scheduler SHALL 只依赖任务/调度所需组件、纯函数和传输契约,SHALL NOT 构造或依赖主服务会话服务、全量业务 Store、Agent、OpenAI、MCP、executor 或用户工作空间挂载。

#### Scenario: Start management without an Agent process
- **WHEN** Scheduler 自身数据库、配置和服务身份有效，但 Agent 执行服务暂时不可用
- **THEN** Scheduler 的健康及已保存任务/触发历史读取可用，不启动本地 Agent
- **AND** 需要主服务校验或执行的操作返回明确的依赖不可用错误

#### Scenario: Verify package and deployment boundaries
- **WHEN** 构建 Scheduler 并检查其依赖与配置
- **THEN** 不需要 LLM key、用户 JWT 签名密钥、工作空间挂载或 Agent 包

#### Scenario: Canonical scheduler binary name and deploy entry
- **WHEN** 构建 Scheduler 入口并检查 deploy/ 目录
- **THEN** 产出二进制名为 blowball-scheduler,其 Dockerfile、Jenkinsfile 与 systemd unit 位于 deploy/scheduler/
