## Context

当前 system prompt 的组装分散在 `internal/agent` 的两个地方：

- `internal/agent/orchestrator.go` 的 `renderSystemPrompt` 负责把 `cfg.SystemPrompt`、environment（userID/platform/OS）、工具列表、skill catalog 拼在一起。
- `internal/agent/prompts.go` 的 `AppendSystemPromptEnv` 负责在 `openai_client.go` 把消息转成 OpenAI 参数时，再追加一段 environment（workspace/platform/OS/knowledge cutoff）。

这导致每个 agent 收到的 system prompt 里出现两个 `# Environment` 段落，内容还互相补充：一段有 userID，一段有 workspace。维护 prompt 时需要同时改两个地方，容易遗漏。

## Goals / Non-Goals

**Goals：**
- 把 system prompt 的渲染逻辑集中到单一模块 `internal/prompt`。
- 消除 environment 提示词的重复注入。
- 让 workspace、userID、platform、OS、cutoff 等信息在单个 `# Environment` 段落里统一呈现。
- 保持 `agent-orchestration` 已有的动态 system prompt 能力不变。

**Non-Goals：**
- 不引入外部模板引擎，模板继续用 Go 字符串拼接。
- 不改变 config.yaml 的 agent 配置结构。
- 不改变 OpenAI 消息格式或 SSE 事件协议。
- 不改动 Confuse/Chongzhi/Liang 的运行时循环逻辑。

## Decisions

### 1. 新建 `internal/prompt` 包，作为纯字符串渲染器（方案 B）

`internal/prompt` 不认识 `tool.Registry` 或 `skill.Loader`，只接收整理好的 `RenderInput`：

```go
type RenderInput struct {
    BasePrompt string
    Workspace  string
    UserID     string
    Platform   string
    OS         string
    Cutoff     string

    Tools  []ToolInfo   // Name, Description, Server（空表示 built-in）
    Skills []SkillInfo  // Name, Description, Location
}

func RenderSystemPrompt(input RenderInput) (string, error)
```

**理由**：
- prompt 包只关心「怎么把数据格式化成字符串」，不涉及工具筛选、MCP 权限、skill 目录等业务规则。
- 单元测试只需要构造几个结构体，不需要 mock registry。
- 业务规则继续留在 orchestrator，符合现有 `buildAgentRegistry` 的职责。

### 2. environment 只在 `RenderSystemPrompt` 中生成一次

`RenderSystemPrompt` 输出的 system prompt 开头包含统一的 `# Environment`：

```markdown
# Environment
- Primary working directory: {workspace}
- Platform: {platform}
- OS: {os}
- User ID: {userID}
- Assistant knowledge cutoff: {cutoff}
```

`internal/agent/openai_client.go` 的 `toOpenAIMessage` 不再调用 `AppendSystemPromptEnv`，只做消息格式转换。

**理由**：
- 唯一责任人原则：prompt 包决定 system prompt 内容，客户端只负责协议适配。
- 消除重复，避免不同层硬编码不一致的 environment 格式。
- workspace 通过显式参数传入，不再依赖 `ctx.Value("workspace")`，更易于测试和追踪。

### 3. orchestrator 负责把 registry/loader 数据转成 `RenderInput`

`orchestratorFactory.renderSystemPrompt` 将被替换为大致如下流程：

1. 用现有 `classifyTools` 逻辑从 `baseRegistry` 中筛选出 built-in tools 和 MCP tools（按 server 分组）。
2. 用现有 skill 过滤逻辑从 `skillLoader` 中筛选允许的 skills。
3. 把结果转成 `[]prompt.ToolInfo` 和 `[]prompt.SkillInfo`。
4. 调用 `prompt.RenderSystemPrompt` 得到最终字符串。

**理由**：
- 工具筛选、权限判断、按 server 分组属于 orchestrator 的编排职责，prompt 包不掌握这些规则。
- 这样 prompt 包对外是“无状态渲染器”，容易被其他非 orchestrator 场景复用（例如测试、未来可能的 prompt 预览接口）。

### 4. 删除 `internal/agent/prompts.go`

`AppendSystemPromptEnv` 的逻辑被合并进 `internal/prompt`，原文件不再保留。

**理由**：
- 避免同一概念在多个文件中重复出现。
- 防止未来有人再次在 `openai_client.go` 里追加 environment。

## Risks / Trade-offs

| Risk | Mitigation |
|---|---|
| 删除 `AppendSystemPromptEnv` 后，如果有其他非 orchestrator 路径也构造了 system message，会丢失 environment。 | 目前只有 orchestrator 路径会生成 system message；实现后全局搜索 `AppendSystemPromptEnv` 和 `renderEnvironment` 确保无遗漏。 |
| `ctx.Value("workspace")` 在 handler 中设置，但 prompt 包不再读取，可能让同事误以为它仍被使用。 | 同步删除 handler 中的 `ctx = context.WithValue(ctx, "workspace", workspaceRoot)`，或保留 TODO 注释说明已废弃。 |
| 现有测试依赖 system prompt 的精确字符串（如 `orchestrator_test.go`）。 | 更新测试断言，使其匹配新的单一 environment 格式；新增 `internal/prompt` 单元测试覆盖各种输入组合。 |
| `classifyTools` 和 skill 过滤逻辑目前和 `renderSystemPrompt` 耦合，拆分后代码量变多。 | 把过滤逻辑提取成 orchestrator 的私有方法，保持 `RenderSystemPrompt` 调用点简洁。 |

## 迁移计划

1. 创建 `internal/prompt/render.go` 和 `internal/prompt/render_test.go`。
2. 在 `orchestrator.go` 中实现 `RenderInput` 组装，替换 `renderSystemPrompt` 和 `renderEnvironment`。
3. 删除 `openai_client.go` 中的 `AppendSystemPromptEnv` 调用。
4. 删除 `internal/agent/prompts.go`。
5. 运行 `go test ./internal/agent/... ./internal/prompt/...`，更新相关断言。
6. 确认 handler 中的 `ctx.Value("workspace")` 是否还有其他用途；若无，一并移除。

## Open Questions

- `ctx.Value("workspace")` 当前只在 `AppendSystemPromptEnv` 中使用，删除后 handler 中设置它的代码也可以删除。是否一并处理？
- knowledge cutoff 目前是硬编码字符串 `"August 2025"`，是否需要在 `RenderInput` 中保留可配置字段，还是继续作为包内常量？
