## Context

blowball 当前的 MCP 子系统是 **operator-global**：`config.yaml` 的 `mcp.servers` 在进程启动时连接所有 server，做 `tools/list`，把每个远端工具注册成进程级 `tool.Registry` 里的代理 `ToolSpec`，由 `AgentFactory.buildAgentRegistry` 按 agent 的 `mcp.servers` 配置过滤后注入。用户无法接入私有 MCP 服务，也无法用自己的凭据认证。

业内多租户 agent 服务端（ChatGPT connectors、Claude connectors、GitHub Copilot 云端、Vertex、Bedrock AgentCore）已一致收敛到一个拓扑：**per-user 远程 HTTP MCP + per-user OAuth/静态凭据**，且**不**允许终端用户在平台服务器上跑 stdio 子进程。本轮调研确认：blowball 的两个既有机制使 per-user 凭据隔离近乎免费成立——

- `internal/handler/message_stream.go:152`：`workspaceRoot := filepath.Join(h.dataDir, userID, "workspace")`，每个 turn 按 userID 解析工作空间根，物理隔离。
- `internal/tool/xizhi/validate.go`：`reservedNamespaceDir = ".blowball"` 被 `validatePath` **硬拒绝**，agent 无法用 `xizhi_*` 读取该命名空间——这天然关闭了「agent 经文件读取窃取用户自有 key」的向量。

per-user skills 已经走在 `.blowball/skills/` 上（`luban_*` 工具族管理）。本设计把 per-user MCP 放到平行的 `.blowball/mcp/`，复用同一套设计语言。

## Goals / Non-Goals

**Goals:**
- 每个用户能在自己的工作空间声明私有 MCP 服务（远程 HTTP + 静态认证）。
- 每个用户用自己的 key 认证，用户间凭据物理隔离、零共享状态。
- agent 能（被用户指派或自主）管理工作空间的 MCP 配置，并按需调用。
- agent 在执行任务前主动评估并选择最合适的 skill 与 MCP 服务。
- 与既有 operator MCP 无冲突地共存。

**Non-Goals:**
- per-user stdio 子进程（维持 operator-only；避免沙箱与进程隔离问题）。
- per-user OAuth 2.1 浏览器授权流程与 token 刷新机制（静态认证即全部范围；OAuth 留作未来扩展，文件届时作为 token 存储）。
- MCP 的 resources / prompts / roots / sampling / elicitation 等长尾能力。
- 跨 turn 的持久连接池（MVP 用 turn-scoped 缓存；池化是未来延迟优化）。
- 工作空间文件内 key 的静态加密（MVP 接受明文落盘 + 信任边界迁移；加密留作未来）。

## Decisions

### D1. 执行模型 = 元工具 `mcp_call`（非「每 turn 代理注册」混合模型）
agent 持有**单个** `mcp_call(server, tool, args)` 元工具按需调用，而非让后端每 turn 读取配置并注册 N 个代理 `ToolSpec`。

- **备选 A（混合模型）**：后端每 turn 读 `.blowball/mcp/config.json`，为该用户 server 注册代理工具，agent 用原生函数调用（schema 校验、可靠性高）。被否决原因：重新引入 per-user registry（两用户都叫 `github` 的工具需命名空间隔离）与**跨 turn 连接池**，而池必须按 `userID` 维度键控——一旦键控错误（如按 serverName），会复用一个用户仍温热的已认证连接去发另一个用户的调用，造成**静默跨用户冒充**。元工具模型无任何跨用户共享状态，该 bug 类根本不存在。
- **元工具的代价**：模型需自行构造 `args`（无 schema 校验，可靠性下降），且需先发现 server 有哪些 tool。代价由 D6（发现策略）与「config 内缓存每个 server 的 `tools/list` schema 并在调用时校验」缓解。
- **结论**：per-user 凭据隔离是本变更的首要目标，元工具在该目标上**结构性更安全**，且机械更简、天然支持动态发现（用户会话中途加 server 即时可见）。可靠性代价可接受且有缓解路径。

### D2. 配置存储 = 工作空间文件 `.blowball/mcp/config.json`
- **备选（DB 表）**：被否决。工作空间文件与 `.blowball/skills/` 设计语言一致、可被 agent 经 `mcp_*` 工具编辑、per-user 隔离由既有 `workspaceRoot` 路径作用域免费提供、无需新数据模型。
- 文件含每个 server 的 `name` / `url` / `transport`（仅 `http`）/ 认证 / 描述；`mcp_add_server` 时一并写入该 server 的 `tools/list` schema 快照供调用校验。

### D3. 传输范围 = 仅远程 HTTP（Streamable HTTP）
per-user **不支持** stdio。stdio 维持 operator-only。
- **理由**：业内一致共识；避免「用户提供的 stdio 命令在共享主机何处 spawn、何种沙箱、何种网络出口」这一整类问题；复用既有 `internal/tool/mcpclient/http.go` 的 transport 与 session-id 管理。

### D4. 认证范围 = 仅静态凭据（bearer / api-key / basic）
- **理由**：工作空间文件无法承载 OAuth 2.1 的浏览器授权舞蹈与刷新机制。静态认证覆盖自托管/内部 MCP 服务（dev workspace 的主要受众）即全部 MVP 范围。
- **未来**：若需 OAuth，文件可作为 token 存储，后端并行增加 OAuth 客户端子系统负责获取/刷新。

### D5. 连接生命周期 = turn-scoped 缓存
turn 开始时按需惰性连接，turn 内复用，turn 结束销毁。
- **备选 A（per-call 临时连接）**：被否决——agent 在一个 turn 内对同一 server 调用多次会重复 connect+initialize（HTTP 约 0.3–1.5s/次），延迟不可接受。
- **备选 B（跨 turn 持久池）**：被否决（MVP）——重新引入 D1 所述的跨用户键控风险与池管理复杂度；其延迟收益对 MVP 非必需。
- turn-scoped 无跨 turn 状态，既分摊 turn 内多次调用的连接成本，又不引入跨用户共享——与 D1 的隔离论证一致。

### D6. 发现策略 = 提示词注入 server 描述 + 按需 `tools/list` + schema 缓存
- 系统提示词注入**server 级**描述（开销小、静态），让 agent 知道有哪些 server。
- `mcp_add_server` 时缓存该 server 的 `tools/list`（工具名 + 入参 schema）到 config 文件。
- `mcp_call` 调用前以缓存 schema 校验 `args`，缓解元工具的可靠性代价；schema 过期则触发一次 `tools/list` 刷新。
- 这把元工具的「模型自由构造 args」风险降到可接受，同时不引入持久代理 registry。

### D7. 超时 = 可配置，默认 10s（total-call）
- `mcp_call` 的超时覆盖**整条调用链**（connect + initialize + 可选 list 刷新 + tools/call）。10s 对快工具充裕，对慢工具（大查询、爬取）会失败——这是有意的保守上限，避免单次工具调用拖垮 turn。
- connect 与 total 分离为两个可配置项（connect 默认更短），避免「握手卡死但 total 未到」的死等。

### D8. 三条泄露不变量（实现强制 + 测试守护）
1. `mcp_list_servers` 返回 name/url/transport/描述，**认证字段一律脱敏**（如返回 `"***"` 或省略）。
2. MCP 客户端结构化日志中，认证 header（`Authorization`、`X-API-Key` 等）**必须脱敏**，不得明文落日志。
3. `mcp_call` 在服务端注入认证，认证值**绝不进入模型可见的输入或输出**。

## Risks / Trade-offs

- [元工具 args 可靠性低于原生函数调用] → D6 的 schema 缓存 + 调用前校验；提示词引导 agent 在不确定工具签名时先 `mcp_list_tools`。
- [明文 key 落盘于 per-user 工作空间文件，operator 可读] → 信任边界迁移（operator 现持有所有用户 key 的明文）。自托管/team 可接受；托管 SaaS 部署需后续静态加密（复用既有 store 层）。文档明确该边界。
- [10s total-call 超时对慢工具过紧] → 超时可配置；接受「慢工具失败」作为有意保守上限。connect/total 分离避免握手死等。
- [MCP 客户端日志泄露认证 header] → D8.2 日志脱敏 + 测试守护（构造带认证 header 的调用，断言日志不含明文）。
- [agent 经 `mcp_*` 工具读到自己的 key 并经其他工具外泄（prompt injection）] → `.blowball/` 已被 xizhi `validatePath` 硬拒绝；`mcp_*` 工具输出按 D8.1 脱敏，key 不进入模型上下文。
- [per-user 与 operator MCP 在提示词/工具列表中两源并存，体验割裂] → 统一在系统提示词中分组渲染（operator 服务与 user 服务分列）；工具命名空间分离（operator 代理工具 vs `mcp_*` 元工具）。
- [config 文件由用户/agent 写入，可能格式错误] → `mcp_*` 工具与服务端读取处都做 schema 校验；坏配置导致该 server 不可用并返回明确错误，**不**崩溃进程（区别于 operator 配置的启动期 fail-fast）。

## Migration Plan

- **纯增量**，无破坏性变更。未配置 `.blowball/mcp/config.json` 的用户行为完全不变。
- operator MCP 路径（`config.yaml`）需求契约不变，并行共存。
- 部署：随版本发布即可；无需数据迁移、无需配置改写。
- **回滚**：移除新增的 `internal/tool/mcp/`、回滚 orchestrator/prompt 的注入点即可；per-user 配置文件留在工作空间无害（不被识别即忽略）。

## Open Questions

1. **`mcp_call` 元工具的 schema 校验失败时行为**：返回结构化错误让 agent 重试，还是直接失败？倾向前者（与现有 tool_error 事件流一致），实现时定。
2. **server 描述注入提示词的上下文成本上限**：用户配置很多 server 时如何裁剪（按描述长度限流？按近期使用频率排序？）。MVP 可先全量注入，观察后再定。
3. **`mcp_list_tools` 是否作为独立工具暴露**：D6 提到「按需 tools/list」。是暴露成独立 agent 工具，还是内化在 `mcp_call` 的 schema 过期刷新里？倾向内化（减少工具表面积），但若 agent 需要主动探索则需独立工具。实现时定。
4. **认证字段在工作空间 REST API（`GET /workspace/files/.blowball/mcp/config.json/content`）的可见性**：用户经 API 看自己的 key 是否可接受（用户自有数据）？MVP 倾向允许（与用户能经 UI 管理一致），但若要严格则需在 workspace API 层对该路径脱敏。留待实现决策。
