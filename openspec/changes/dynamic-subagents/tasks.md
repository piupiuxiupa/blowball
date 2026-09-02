## 1. 配置与数据模型基础

- [x] 1.1 在 `internal/config` 增加 `agents.subagent` 段（默认 system_prompt/tools/mcp/skills/max_rounds + `max_depth` 默认 1、`max_concurrent`、`max_total_per_turn` + 可选 `presets`），加载器拒绝残留的 `agents.chongzhi` / `agents.liang` 段并给出指向迁移的错误信息；同步更新 `config.example.yaml`；验证：新增配置单测覆盖合法加载、残留段拒绝、预算校验（`max_concurrent > max_depth`）
- [x] 1.2 在 `internal/model` 为 Message/StreamEvent Meta 增加可选 `agent_instance_id`（与既有 `parent_tool_call_id` 并存）；验证：模型层单测确认 JSON 序列化 omitempty 且存量载荷不变

## 2. 持久化层

- [x] 2.1 新增顺序命名 migration：`messages` 加 nullable `agent_instance_id` 列；新建 `subagent_runs` 表（session 级联删除，含 instance/parent/depth/name/tools/status/messages_json）；验证：migration 在空库与含存量数据的库上均可重复应用，会话删除级联单测通过
- [x] 2.2 在 store 层实现子 Agent 快照读写：Run 结束按 instance upsert 全量 `messages_json`、按 `(session_id, agent_instance_id)` 读取、超限标记不可续跑；验证：store 单测覆盖 upsert 幂等、跨会话读取拒绝、超限标记

## 3. 通用子 Agent 实现

- [x] 3.1 将 Chongzhi/Liang 循环合并为参数化通用子 Agent（SubAgentSpec：system_prompt、scoped 工具集、max_rounds、深度），保留 ToolCallTracker/RoundCapTracker 能力接口；验证：通用实现的既有行为等价单测（loop、retry、cap、length continuation）全部迁移并通过
- [x] 3.2 实现 `spawn_subagent` 参数契约（task 必填；context/name/tools/preset/resume_agent_id 可选）与校验：task 缺失、tools 越权扩权、未知 preset、非法 resume 目标均返回 bad_args 错误 tool result；验证：参数解析与校验表驱动单测
- [x] 3.3 实现派发时工具收窄：从父有效工具集派生 scoped registry（MCP 引用裁剪不重建连接），未提供 tools 时继承父集（扣除 spawn 工具，除非深度预算允许）；验证：收窄/继承/深度剔除 spawn 的 registry 构建单测

## 4. 调度、身份与预算

- [x] 4.1 改造 Confucius dispatch：单一 `spawn_subagent` 拦截（替换 invoke_*），生成稳定 instance id，事件注入升级为双字段 view（instance + run）；验证：调度单测确认旧 invoke 工具不存在、事件身份成对出现
- [x] 4.2 实现结构化结果契约：tool result 末尾附带 agent_id 与 status（completed/capped/error）标记，capped 由 RoundCapTracker 驱动，error 保持错误+部分输出合并语义；验证：三种 status 的结果构造单测（含字节级回归断言）
- [x] 4.3 实现全树 treeBudget：加权并发信号量（等待父链占槽）、原子 total 计数、深度预算；超并发 FIFO 等待 + 超时返回预算错误；超 total 立即失败；验证：预算控制器单测（并发上限、总数上限、等待超时、深度边界）
- [x] 4.4 实现嵌套派发：深度预算内子 Agent 获得 spawn 工具并递归构建孙 Agent，共用同一 treeBudget 与事件流（父链归因）；验证：fake client 驱动的两层嵌套集成单测（depth=1 无工具、depth=2 正常嵌套）

## 5. 续跑

- [x] 5.1 Run 结束（成功/失败/capped）写入历史快照，续跑派发以快照 + 新任务消息为初始上下文，沿用同 instance id、分配新 run id；验证：快照写入与续跑上下文构造单测（含失败实例可续跑）
- [x] 5.2 实现续跑边界：跨会话/不存在/超限快照拒绝，孙 Agent 不复活（历史中仅存 tool result），续跑内可派新孙 Agent 受剩余预算约束；验证：边界场景表驱动单测

## 6. 观测、契约与提示词

- [x] 6.1 usage 归因升级：`by_agent` key 为动态实例标签、续跑累计同 key，`sub_agent_invocations` 每项含实例 id/父实例/派发顺序；验证：TurnBreakdown 构造与 done 事件形状单测
- [x] 6.2 更新 `api/openapi.yaml`：SSE 事件 Meta 新增 `agent_instance_id`（及父链字段）契约说明；验证：OpenAPI lint/校验通过且与 stream 实现字段一致
- [x] 6.3 在 Confucius system prompt 增加派发纪律段（自包含 task prompt、并行/有界/独立优先、小任务自营、写集不相交、capped 可续跑）；验证：prompt 渲染快照测试含新段落

## 7. 集成与回归

- [x] 7.1 端到端集成测试（`test/integration/`）：并行多 spawn、工具收窄、续跑、深度=1 默认行为等价、预算耗尽路径；验证：`go test ./test/integration/...` 通过
- [x] 7.2 全量回归：`make test`（含 race）与 `make lint` 通过；确认存量 NULL 身份消息行读取与展示退化路径无回归
