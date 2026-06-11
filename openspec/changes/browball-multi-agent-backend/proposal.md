## Why

需要搭建一套名为 blowball 的多用户多 Agent 后端服务，支持智能任务分发、并行执行和流式响应。当前项目从零开始，需要建立完整的基础架构，包括用户鉴权、会话管理、Agent 编排引擎、文件操作工具集和数据持久化层。

## What Changes

- 新建 Go 项目骨架，引入 gin、openai-go、zap、go-landlock、sqlx、go-redis、jwt、a2a-go 等依赖
- 实现用户登录与 JWT 鉴权体系
- 实现 SSE 流式响应，支持多 Agent 并行透传
- 实现 Agent 编排引擎：Confuse 作为唯一调度者，通过 function-calling 调度 Chongzhi（编码）和 Liang（分析）两个子 Agent
- 实现文件操作工具集 Xizhi，通过 go-landlock 限制写入范围到用户工作空间
- 实现三层消息存储：Redis 缓存 → 用户目录文件 → MySQL 持久化
- 实现会话标题自动生成（调用模型根据问答内容生成简短标题）
- 设计并创建 MySQL 数据表（users、sessions、titles、messages），所有表包含 trace_id 和 update_time
- 提供完整 API 接口（登录、会话消息、会话列表、MCP 工具列表、skills 列表、工作空间文件管理、文件上传下载）
- Agent 配置（system prompt、model、tools）写入配置文件，支持动态调整
- 具备可观测能力：记录每次请求的 token 用量和 trace_id

## Capabilities

### New Capabilities

- `user-auth`: 用户注册登录、JWT 签发验证、鉴权中间件
- `session-management`: 会话创建、消息收发、会话列表、标题自动生成、三层消息存储与恢复
- `agent-orchestration`: Agent 编排引擎，Confuse 调度 Chongzhi/Liang，支持并行透传流式响应、Agent-as-Tool 模式
- `xizhi-tools`: 文件操作工具集（读/写/修改），go-landlock 进程级限制 + 应用层路径校验，用户工作空间隔离
- `workspace-api`: 用户工作空间文件管理、文件上传下载、目录列表、文件内容读取
- `api-server`: Gin HTTP 服务、路由注册、SSE 流式输出、统一错误处理、CORS

### Modified Capabilities

（无，全新项目）

## Impact

- 全新 Go 项目，无现有代码影响
- 外部依赖：
  - github.com/gin-gonic/gin
  - github.com/openai/openai-go/v3
  - go.uber.org/zap
  - github.com/landlock-lsm/go-landlock
  - github.com/jmoiron/sqlx
  - github.com/redis/go-redis/v9
  - github.com/golang-jwt/jwt/v5
  - github.com/a2aproject/a2a-go/v2
- 基础设施依赖：MySQL、Redis
- 需要设计 4 张数据库表及对应 migration
- 用户数据目录结构：`data/{user_uuid}/sessions|workspace|skills`
