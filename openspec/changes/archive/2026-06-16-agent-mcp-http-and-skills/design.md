## Context

Blowball 当前已完成 `add-mcp-client-support`：支持通过 SSE 和 stdio 连接外部 MCP 服务，远端工具在启动时被发现并注册到进程级 `tool.Registry`，随后由 `Orchestrator.AgentFactory` 复制到每次请求的 `reqReg`。

当前存在以下限制：
- 所有 agent 看到同一套 MCP 工具，无法按 agent 精细控制。
- 系统提示词是静态字符串，模型不知道当前有哪些 MCP 工具、哪些 skill 可用。
- 不支持 MCP Streamable HTTP 传输。
- Skill 仅按 per-user 平铺文件扫描，没有全局 skill，也没有 agentskills 规范的 `SKILL.md` 结构。

本设计在现有 `tool.Registry` + `AgentFactory` 架构上扩展，不改动 LLM 调用层和 agent 循环核心逻辑。

## Goals / Non-Goals

**Goals:**
- 支持 MCP Streamable HTTP 传输，含 `Mcp-Session-Id` 缓存与过期自动重初始化。
- 允许按 agent 配置可访问的 MCP server 及工具白名单。
- 允许按 agent 配置可用的 skill 列表，skill 来源包括全局目录和当前用户目录。
- 把可用工具（内置 + MCP）和可用 skill 以 XML 形式注入每个 agent 的系统提示词。
- 提供 `read_skill` 工具，让 agent 按名称加载 skill 完整内容。
- 对齐 agentskills 规范：skill 采用 `{skill-name}/SKILL.md` 子目录结构，frontmatter 包含 `name` 与 `description`。

**Non-Goals:**
- 让 blowball 自身成为 MCP server。
- 支持 MCP resources、prompts、sampling、roots 等非工具能力。
- 运行时动态添加/删除 MCP server 或 skill（配置变更需重启）。
- 自动把 skill 全文预加载到上下文（采用 progressive disclosure：只注入 catalog，使用时由模型调用 `read_skill`）。
- 对 skill 内容做向量化检索或语义匹配。

## Decisions

### 1. HTTP transport 作为 `Transport` 的第三种实现
**Decision:** 在 `internal/tool/mcpclient` 中新增 `HTTPTransport`，与 `SSETransport`、`StdioTransport` 实现同一 `Transport` 接口。
**Rationale:**
- SSE 和 HTTP 共享 JSON-RPC 协议层，差异只在底层 I/O。
- `Client` 不需要知道传输细节，session 管理封装在 `HTTPTransport` 内部。
- 便于测试：可用 mock transport 验证 `Client` 的调用转发。

### 2. Session 生命周期由 `HTTPTransport` 自行管理
**Decision:** `HTTPTransport` 内部维护 `sessionID`；首次 `Initialize` 后缓存响应头 `Mcp-Session-Id`；后续请求自动附加该 header；收到 session 失效响应时自动重新 `Initialize` 并重试一次。
**Rationale:**
- 保持 `Client` 简单，不感知 HTTP 传输特有的 session 语义。
- 自动重初始化对调用方透明，避免单次 session 过期导致整个 agent turn 失败。
- 用 `sync.RWMutex` 保护 `sessionID`，并发安全。

### 3. 保留 server → tools 映射以支持按 agent 过滤
**Decision:** `mcpclient.RegisterAll` 除了注册代理 `ToolSpec` 外，还在 `Manager` 或独立结构中维护 `map[serverName][]toolName`。`AgentFactory.Build` 从该映射中查询工具归属，结合 agent 的 `mcp.servers` 配置过滤。
**Rationale:**
- 代理工具名可能被 `prefix` 改写，不能仅从工具名反推 server。
- `ToolSpec` 本身无 server 元数据字段，避免污染通用工具抽象。
- 集中维护映射便于校验和错误报告。

### 4. Agent MCP 配置采用 server + tools 白名单
**Decision:** 配置结构为 `agents.*.mcp.servers[].name` + `agents.*.mcp.servers[].tools`，`tools: ["*"]` 表示允许该 server 全部工具。
**Rationale:**
- 比单纯工具名列表更清晰，便于在系统提示词中按 server 分组展示。
- 与 `mcp.servers` 的声明式配置语义一致。
- 如果 server 名写错，启动时即可校验失败。

### 5. Skill 目录结构对齐 agentskills 规范
**Decision:** 全局 skill 位于 `{project_root}/skills/{skill-name}/SKILL.md`，用户 skill 位于 `{data}/{userID}/skills/{skill-name}/SKILL.md`；每个 `SKILL.md` 顶部包含 YAML frontmatter，至少包含 `name` 和 `description`。
**Rationale:**
- 与用户引用的 agentskills.io 文档保持一致，便于未来跨客户端共享。
- 子目录结构允许 skill 携带脚本、引用等辅助资源。
- frontmatter 提供稳定的 `description` 用于系统提示词注入。

### 6. 提供专用 `read_skill` 工具
**Decision:** 新增 `internal/tool/skill` 包，实现 skill 发现、frontmatter 解析、`read_skill` 工具注册。
**Rationale:**
- 现有 `xizhi_read_file` 被限制在用户 workspace 内，无法读取 workspace 外的 skill 文件。
- 专用工具可以隐藏绝对路径，按 name 查找，未来可扩展权限控制、缓存、内容包装。
- 模型看到 skill catalog 后，自然地调用 `read_skill(name)` 加载指令。

### 7. System prompt 在 `AgentFactory.Build` 时动态构建
**Decision:** 把 `AgentFactory.Build(workspaceRoot string)` 改为 `Build(workspaceRoot, userID string)`，在构建每个 agent 时：
1. 按 agent 的 `tools` 和 `mcp` 配置过滤出可用工具；
2. 按 agent 的 `skills` 配置加载全局 + 用户 skill 元数据；
3. 把 `cfg.SystemPrompt` + 环境信息 + 可用工具列表 + skill XML catalog + 使用说明拼接成完整 system prompt；
4. 将完整 system prompt 存到 agent 实例中，`SystemPrompt()` 返回该值。
**Rationale:**
- per-user skill 必须在请求时加载，无法在启动时缓存。
- 一次构建、多次使用，避免每个 LLM round 重复渲染。
- 与现有 `withSystem` 机制兼容，无需修改 OpenAI 调用层。

### 8. 可用工具按 server 分组注入
**Decision:** 系统提示词中的工具段落分为 `Built-in Tools` 和 `MCP Tools`；MCP 工具按 server 分组列出。
**Rationale:**
- 内置工具与 MCP 工具来源不同，分组便于模型理解。
- 按 server 分组与 agent MCP 配置的语义一致。
- 模型需要知道工具名和描述即可，不需要完整 JSON schema。

### 9. Skill catalog 使用 XML 格式
**Decision:** 系统提示词中的 skill 段落使用 XML 标签 `<skills>` / `<skill>` / `<name>` / `<description>` / `<location>`，并附带简短使用说明。
**Rationale:**
- 与用户引用的 agentskills.io 推荐格式一致。
- XML 结构对模型清晰，便于解析和引用。
- `<location>` 保留为可选信息，未来如果专用工具完全隐藏路径，可省略。

## Risks / Trade-offs

| Risk | Mitigation |
|---|---|
| HTTP MCP server 的 session 语义实现各异 | 严格按 MCP Streamable HTTP spec 处理 `Mcp-Session-Id`，并在失败时返回明确错误；对不规范 server 提供 `skip_session` 配置项作为逃生口。 |
| Agent MCP 配置中的 server 名或工具名写错 | 启动校验：server 名必须在 `mcp.servers` 中存在；工具名必须在远端 `tools/list` 中存在（或 `"*"`）。 |
| Skill 目录扫描过深或文件过多 | 设置最大扫描深度（如 4 层）和最大 skill 数量（如 200），避免启动/请求耗时过长。 |
| `read_skill` 读取大文件导致 token 爆炸 | 限制单 skill 文件大小（如 500KB）；超限返回错误，不注入上下文。 |
| 用户 workspace 与 skill 目录的权限边界混淆 | `read_skill` 独立实现，不借用 `xizhi_read_file`；skill 目录只读，不写回 workspace。 |
| 系统提示词过长 | 只注入 name + description，不注入 skill 全文；MCP 工具只注入 name + description，不注入 JSON schema。 |
| 现有 per-user 平铺 skill 文件需要迁移 | 在 proposal 中标记为迁移项；实现阶段提供一次性迁移说明文档。 |

## Migration Plan

1. **配置迁移**
   - 在 `config.example.yaml` 中展示新的 `agents.*.mcp` 和 `agents.*.skills` 字段。
   - 现有 `config.yaml` 不强制立即修改：若新字段缺失，则 agent 默认不启用任何 MCP 工具或 skill。

2. **Skill 文件迁移**
   - 现有 `{data}/{userID}/skills/*.md` 平铺文件需手动迁移到 `{data}/{userID}/skills/{name}/SKILL.md`，并添加 frontmatter。
   - 全局 skill 新建 `{project_root}/skills/` 目录。

3. **代码迁移**
   - `AgentFactory.Build` 的调用点只有 `Orchestrator.Handle` 和 handler adapter，同步修改即可。
   - `GET /api/v1/skills` 的响应结构保持不变（`name, filename, size, update_time`），但扫描方式改变。

4. **Rollback**
   - 移除 `agents.*.mcp` 和 `agents.*.skills` 配置即可恢复旧行为。
   - 若 HTTP transport 引入问题，可回退到 SSE/stdio。

## Open Questions

- `read_skill` 返回内容时是否剥离 YAML frontmatter？建议剥离，只返回 markdown body。
- 全局 skill 目录路径是否可配置？建议第一版硬编码为 `{project_root}/skills/`，后续再暴露 `skills.global_dir`。
- 当用户 skill 与全局 skill 同名时， precedence 规则是什么？建议用户 skill 覆盖全局 skill，与 agentskills 的 project-level override 一致。
- HTTP MCP server 如果根本不返回 `Mcp-Session-Id`，是否允许？建议允许，按无 session 处理。
