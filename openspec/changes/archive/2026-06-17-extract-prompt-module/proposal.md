## Why

`internal/agent/prompts.go` 和 `internal/agent/orchestrator.go` 各自维护了一段 environment 提示词，导致最终 system prompt 里出现两个 `# Environment` 段落：一段包含 workspace 和 knowledge cutoff，另一段包含 userID。这种重复既浪费 token，也让 prompt 的维护变得困难。把 prompt 渲染逻辑拆成独立模块，可以明确唯一责任人，并为后续扩展（新模板、多语言、A/B 测试 prompt）打下基础。

## What Changes

- 新建 `internal/prompt` 包，作为 system prompt 渲染的唯一责任人。
- `internal/prompt` 提供纯数据渲染 API：`RenderSystemPrompt(RenderInput) (string, error)`，输入只包含字符串和简单结构体，不依赖 `tool.Registry` 或 `skill.Loader`。
- `internal/agent/orchestrator.go` 中的 `renderSystemPrompt` 和 `renderEnvironment` 移除，改为调用 `internal/prompt.RenderSystemPrompt`。
- `internal/agent/orchestrator.go` 负责从 `tool.Registry`、`skill.Loader` 和配置中筛选/收集工具与技能，再把纯数据传给 prompt 包。
- `internal/agent/openai_client.go` 停止调用 `AppendSystemPromptEnv`，不再在消息转换阶段追加 environment。
- 删除 `internal/agent/prompts.go`。
- environment 统一成单个 `# Environment` 段落，包含：Primary working directory、Platform、OS、User ID、Assistant knowledge cutoff。
- 新增 `internal/prompt` 单元测试，并更新 `orchestrator_test.go` 与 `openai_client_test.go` 中断言的 system prompt 内容。

## Capabilities

### New Capabilities
- `system-prompt-rendering`: 集中式 system prompt 渲染能力，提供 environment、工具列表、skill catalog 的统一模板渲染。

### Modified Capabilities
- 无。`agent-orchestration` 的要求（动态 system prompt 构建、按 agent 过滤工具/skill）保持不变；本次是内部实现重构。

## Impact

- 受影响文件：`internal/agent/orchestrator.go`、`internal/agent/openai_client.go`、`internal/agent/prompts.go`。
- 新增文件：`internal/prompt/render.go`、`internal/prompt/render_test.go`。
- 行为变化：最终 system prompt 只包含一个 environment 段落；`ctx.Value("workspace")` 不再被 prompt 渲染读取，workspace 通过显式参数传递。
- 无 API 或配置格式变更。
