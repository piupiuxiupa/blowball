## 1. 配置模型与加载器

- [ ] 1.1 定义 per-user MCP 配置的 Go 结构（`internal/tool/mcp/config.go`）：server 条目含 `name`/`url`/`transport`/`auth`/`description`，以及 `tools/list` 缓存（工具名 + 入参 schema）
- [ ] 1.2 实现配置加载器：从 `{workspaceRoot}/.blowball/mcp/config.json` 读取/反序列化；文件缺失视为空（无 server），不报错
- [ ] 1.3 实现配置校验：拒绝 `transport != http` 的条目；拒绝要求 OAuth 流程的 `auth`；缺必需字段返回明确错误（不 panic）
- [ ] 1.4 实现配置写入（供管理工具用）：原子写回 `.blowball/mcp/config.json`，重名校验，保留其他条目

## 2. per-user turn-scoped 连接管理

- [ ] 2.1 实现 turn-scoped 连接管理器：按 turn 生命周期持有 `{serverName → *mcpclient.Client}`，惰性连接、turn 内复用、turn 结束销毁（复用 `internal/tool/mcpclient/http.go` transport 与 session-id 管理）
- [ ] 2.2 实现 connect 与 total-call 分离的超时（默认 total 10s），握手死等由 connect 超时兜底
- [ ] 2.3 复用既有 SSE-reconnect/HTTP re-init 逻辑处理单 turn 内的连接异常，重连失败返回错误

## 3. `mcp_*` 管理工具族

- [ ] 3.1 实现 `mcp_list_servers`：返回该用户已配置 server 的 name/url/transport/description，**认证字段脱敏**
- [ ] 3.2 实现 `mcp_add_server`：校验并写入配置，连接该 server 拉取 `tools/list` 缓存到配置文件；重名拒绝
- [ ] 3.3 实现 `mcp_remove_server`：按 name 移除条目，保留其余
- [ ] 3.4 工具均经 context 取 `userID` 并定位该用户工作空间（仿 `luban` 的 `userDirFn(userID)` 模式）；返回结果不含认证明文

## 4. `mcp_call` 调用工具

- [ ] 4.1 实现 `mcp_call(server, tool, args)`：经 turn-scoped 管理器取连接，转发 `tools/call`，返回结果；远端 `isError=true` 转为 tool_error
- [ ] 4.2 调用前校验：`tool` 须在缓存 `tools/list` 中，否则调用前即拒绝；`args` 须符合缓存入参 schema，否则调用前即拒绝
- [ ] 4.3 schema 过期检测：可选触发一次 `tools/list` 刷新缓存后再校验/调用
- [ ] 4.4 认证在进程内注入到出站请求，认证值不进入模型可见的输入/输出

## 5. 泄露不变量（脱敏）

- [ ] 5.1 `mcp_list_servers` / `mcp_add_server` / `mcp_remove_server` 输出对认证字段统一脱敏（`"***"` 或省略），加单测断言无明文
- [ ] 5.2 MCP 客户端结构化日志对 `Authorization`/`X-API-Key` 等认证 header 脱敏；加测试构造带认证调用并断言日志无明文
- [ ] 5.3 审查 `internal/tool/mcpclient/` 既有日志路径，确保 per-user 路径复用时不会回显认证

## 6. 提示词集成

- [ ] 6.1 在 `internal/prompt/render.go` 渲染该用户 per-user MCP 服务（server 级 name + description），与 operator MCP 分组分列
- [ ] 6.2 加入「执行任务前主动评估并选择最合适 skill 与 MCP 服务」的引导文本
- [ ] 6.3 加入「`.blowball/mcp/` 仅由 `mcp_*` 工具管理，不得经 `xizhi_*` 访问」约束行（与既有 `.blowball/skills/` 约束并列）
- [ ] 6.4 更新 `render_test.go` 覆盖上述三类新增文本

## 7. Orchestrator 与装配

- [ ] 7.1 在 `internal/agent/orchestrator.go` 的 `buildAgentRegistry` / `collectTools` 中注入 `mcp_*` 工具族与 per-user MCP 服务描述（per-user 路径按 turn 解析 `workspaceRoot`/`userID`）
- [ ] 7.2 在 `cmd/blowball/serve.go` 仿 `needsLubanTools` 为 agent 角色装配 `mcp_*` 工具族（api 角色不需要）
- [ ] 7.3 确认 per-user 与 operator MCP 在工具列表/提示词中分组共存、命名空间不冲突

## 8. 测试与集成

- [ ] 8.1 `internal/tool/mcp/` 单测：配置加载/校验/写入、turn-scoped 连接复用、`mcp_call` schema 校验与超时、脱敏
- [ ] 8.2 隔离性测试：构造两用户的 turn，断言一方的配置/连接/认证不进入另一方路径
- [ ] 8.3 `test/integration/`：真实 orchestrator + fake MCP server，覆盖「用户加 server → agent 经 `mcp_call` 调用 → 结果回传」全链路
- [ ] 8.4 文档：更新 `CLAUDE.md` 的 MCP 章节（per-user 路径、范围限定、三条泄露不变量、与 operator 共存）
