## Context

blowball 是一个全新的 Go 多用户多 Agent 后端服务。项目从零开始，需要建立完整的用户鉴权、会话管理、Agent 编排、文件操作和数据持久化体系。技术栈已确定：Gin (HTTP)、openai-go/v3 (LLM 调用)、zap (日志)、go-landlock (文件权限)、sqlx (MySQL)、go-redis/v9 (缓存)、jwt/v5 (鉴权)、a2a-go/v2 (预留外部 Agent 通信)。

## Goals / Non-Goals

**Goals:**
- 建立清晰的分层架构（handler → service → agent/tool → store），方便后续维护和扩展
- 实现 flat 拓扑的 Agent 编排引擎，Confuse 作为唯一调度者
- 支持多 Agent 并行执行 + SSE 流式透传，LLM 自主决定是否并行
- 实现三层消息存储（Redis → 用户目录文件 → MySQL），兼顾性能和可靠性
- 通过 go-landlock + 应用层路径校验实现用户工作空间隔离
- 具备可观测能力：trace_id 贯穿请求链路，记录 token 用量
- 每个模块有对应的单元测试

**Non-Goals:**
- 不实现 Agent 嵌套调用（子 Agent 不能调度其他 Agent）
- 不实现长对话上下文截断/摘要（先保持简单，遇到 token 限制再处理）
- 不实现用户注册（用户数据由外部系统管理或手动入库）
- 不实现 WebSocket（仅 SSE 流式）
- 不实现 A2A 协议的具体功能（仅引入依赖预留）
- 不实现 Skills 的动态加载（skills 目录预留，但 API 只返回静态列表）

## Decisions

### 1. 项目分层架构

```
cmd/server/main.go          ← 入口
internal/
  config/                    ← 配置加载 (config.yaml)
  handler/                   ← Gin HTTP handler（薄层，只做参数解析和响应）
  middleware/                 ← JWT 鉴权、CORS
  service/                   ← 业务逻辑（session 管理、消息保存/恢复、标题生成）
  agent/                     ← Agent 编排引擎（orchestrator、confuse、chongzhi、liang）
  tool/                      ← 工具注册表 + Xizhi 实现
  stream/                    ← StreamEvent、SSE 写入器、并发 channel 管理
  store/mysql/               ← MySQL 数据访问 (sqlx)
  store/redis/               ← Redis 缓存 (go-redis/v9)
  store/fs/                  ← 用户目录文件读写
  model/                     ← 数据模型定义
  pkg/jwt/                   ← JWT 工具
  pkg/logger/                ← Zap 初始化
  pkg/trace/                 ← trace_id 生成
```

**理由**：handler 薄、service 厚的分层让业务逻辑可测试。agent 包独立于 HTTP 层，可单独演进。store 层按存储引擎分包，service 层通过接口解耦。

### 2. Agent 编排引擎

Confuse 通过 OpenAI function-calling 调度子 Agent。执行流程：

1. 用户消息进入 Confuse 的 Agent Loop
2. Confuse 调用 OpenAI streaming API（带 agent_tools）
3. 如果 LLM 返回多个 tool_calls → `errgroup` 并行执行
4. 每个 tool_call 对应一个子 Agent 调用，子 Agent 在独立上下文中运行
5. 子 Agent 的 token 流通过共享 `chan StreamEvent` 透传给 SSE
6. 错误也作为 StreamEvent 流式通知
7. 所有并行调用完成后，结果返回 Confuse 的 tool response
8. Confuse 进行下一轮（可能继续调用或直接回复）

**替代方案**：
- 串行执行所有 tool_calls → 简单但延迟高
- 用独立 HTTP/gRPC 调用子 Agent → 过度工程化

### 3. 流式透传架构

```go
type StreamEvent struct {
    Type    string                 // agent_start | token | tool_call | agent_end | agent_error | done
    Agent   string                 // Confuse | Chongzhi | Liang
    Content string
    Meta    map[string]interface{} // tokens_used, trace_id, tool_name, error_code, etc.
}
```

所有 Agent 共享一个 `chan StreamEvent`（buffered 256），SSE handler 消费并写入 HTTP 响应。Agent 切换通过 `agent_start`/`agent_end` 事件标记，前端可据此做 UI 切换。

### 4. 三层消息存储

```
写入路径: 消息 → Redis (SET with TTL) → 用户 sessions/ 目录 (JSON) → MySQL
读取路径: Redis (命中?) → 用户 sessions/ 目录 → MySQL (兜底)
```

Redis 作为热缓存，过期时间 configurable（默认 24h）。用户目录文件作为温存储。MySQL 作为持久化最终存储。所有写入先写 Redis，然后异步写文件和 MySQL。

**理由**：兼顾实时性能和可靠性。Redis 挂了不影响（降级到文件读取）。文件也丢了还有 MySQL。

### 5. 会话标题生成

用户首条消息发送且Agent回复后，异步调用 OpenAI 生成简短标题（不阻塞流式响应）。标题写入 title 表，同时更新 Redis 缓存。

### 6. 文件权限隔离

双层防护：
- **go-landlock**：进程级限制，整个进程只能读写 `data/` 目录
- **应用层校验**：Xizhi 每次操作前 `strings.HasPrefix(absPath, userWorkspacePath)`，确保只能在 `data/{user_uuid}/workspace/` 内操作

### 7. 配置管理

Agent 的 system_prompt、model、max_tokens、tools 列表全部写入 config.yaml。服务启动时加载，运行时不可修改。后续可扩展为热加载。

### 8. 数据库设计

4 张表，所有表包含 `trace_id VARCHAR(36)` 和 `update_time TIMESTAMP`：

- `users` (user_id PK UUID, username, password, status)
- `sessions` (user_id, session_id PK UUID, trace_id, update_time)
- `titles` (session_id PK, title, trace_id, update_time)
- `messages` (id AUTO_INCREMENT, session_id, msg_time, agent, msg_index, role, content, trace_id, update_time)

## Risks / Trade-offs

- **[并行流式 channel 竞争]** → 多 Agent 并行写入同一 channel，需保证事件顺序可辨识。通过 StreamEvent.Agent 字段标记来源，前端按 agent 分区渲染。
- **[go-landlock 限制范围]** → landlock 一旦设置无法放宽。如果在 main.go 初始化时设置，所有 goroutine 共享限制。需确保日志输出等不被阻断。Mitigation：landlock 仅限制写操作，不影响读；日志可输出到 stdout。
- **[子 Agent 上下文丢失]** → 独立上下文意味着子 Agent 看不到历史对话。如果任务需要历史上下文，Confuse 必须在 task description 中提炼关键信息。Mitigation：Confuse 的 system prompt 明确要求传递必要上下文。
- **[SSE 连接断开]** → 客户端断开后服务端 goroutine 仍在运行。Mitigation：通过 context 取消机制传播断开信号，goroutine 检测 ctx.Done() 后退出。
- **[MySQL 异步写入丢失]** → Redis 写入成功但 MySQL 写入失败时，数据暂存于 Redis 和文件。Mitigation：文件作为中间层保障，MySQL 写入可重试。
