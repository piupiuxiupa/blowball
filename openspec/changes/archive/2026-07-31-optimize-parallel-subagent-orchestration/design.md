## Context

blowball 的多 agent 系统以 Confucius 为中心，通过 OpenAI parallel function-calling 调度 Chongzhi（写代码）与 Liang（分析）子 agent。当前并行机制的数据流：

```
dispatchSubAgent 捕获 toolResult.subUsage  ──✅ 数据在源头齐全
        │
  Confucius.Run: total.Add(*subUsage)        ──❌ 折回父总量，per-agent 拆分丢失
        │
  doneUsage{ confucius: usage }              ──❌ 仅单个 Usage
        │
  emitDone → 扁平 {prompt,completion,total}   ──❌ 无 by_agent（注释自称有，与实现矛盾）
        │
  ports.go: done 事件显式排除出持久化          ──❌ 成本永不落库
        │
  messages 表：零 usage 列                    ──❌ 无历史成本
```

关键约束：
- `agent-orchestration` spec 的 "Token usage observability" 已要求 done 事件含 per-agent 明细，实现未兑现——本变更 B 部分是 spec 合规修复。
- 子 agent 返回自由文本 `finalContent`，invoke tool schema 仅定义输入 `{task,context}`。
- 错误一律 `toolResult{content: err.Error(), isError:true}` 喂回模型，无重试、无错误分类。
- OpenAI tool-calling 协议要求下一轮 completion 拿到本轮全部 tool_call 结果——这决定了"流式 join"在协议层不可行。
- 现有持久化三层（Redis → FS → MySQL），`SaveMessagesBatch` 是写入咽喉点；done 事件被刻意排除以保持消息日志干净。

利益相关方：运营（成本可控可查）、前端（done 事件 usage 形状）、Confucius（综合质量）、子 agent（重试/幂等）。

## Goals / Non-Goals

**Goals:**
- done 事件发射真实 per-agent usage 明细（`by_agent` map），兑现 spec 与现有注释承诺。
- per-turn usage 持久化到专用 `turn_usage` 表，随消息批次同事务写入，保持消息日志干净；支持历史成本与 per-session 汇总。
- 为 Liang 提供可选 structured output（`response_format: json_schema`），提升 Confucius 综合 join 质量。
- 对子 agent 瞬时错误做受限重试，per-agent 幂等感知（Liang 可重试；Chongzhi 仅在无 tool_call 时）。
- 在 Confucius 系统 prompt 注入并行决策指导，并用 turn_cost_tracking 度量其效果。
- 向后兼容：done 事件 usage 形状扩展而非破坏；新配置项全部可选带默认值。

**Non-Goals:**
- **流式 join / 部分结果降级**：OpenAI 协议要求本轮所有 tool_call 结果齐备才能开下一轮 completion，真正流式不可行。turn 延迟已是 `max(子agent延迟)`，吞吐收益已兑现。未来若有具体 MCP 长尾痛点，以"per-sub-agent 超时"归入失败处理。
- **递归子 agent（二级派发）**：维持 flat 拓扑，子 agent 不可再调度。
- **自动成本护栏（超 N token 中止）**：本变只提供数据；运行时中止策略留待后续。
- **Chongzhi 的 structured output**：其输出本质是文件改动，结构化收益低。
- **usage 的 Redis/FS 缓存层**：usage 走 MySQL 单层即可，无需热缓存。
- **turn_usage 的删除归档镜像表**：本变先不加 `turn_usage_deleted`（`008_deletion_archive.sql` 风格），随 sessions cascade 即可；如需审计级保留后续补。

## Decisions

### D1: per-agent usage 以 map 沿调用链传递，不改 Agent 接口签名
**Choice:** `Confucius.Run` 在 dispatch 循环内组装 `byAgent map[string]Usage`（含 Confucius 自身），通过新增的返回值或包装结构传给 `Orchestrator.Handle → emitDone`。叶子 agent（Chongzhi/Liang）的 `Run` 签名不变——它们返回自身 usage，父级负责汇总。
**Rationale:** 数据已在 `toolResult.subUsage`，只是被 `total.Add` 压扁。改 `Agent.Run` 接口签名（所有 agent + 全部测试）代价大且无必要——只有 Confucius 需要聚合，叶子 agent 无子级可聚合。新增一个返回值（或 `RunResult` 结构）影响面可控。
**Alternatives considered:**
- *改 `Agent.Run` 返回 `RunResult{Content, Usage, ByAgent}`*：类型更清晰，但触动所有 agent 实现 + 测试 mock。若后续递归落地再升级到此结构。
- *在 Hub 上额外发 usage 事件*：污染事件流、与"done 事件承载 usage"的现有契约冲突。弃。

### D2: 新建 `turn_usage` 表，随 SaveMessagesBatch 同事务写入
**Choice:** `turn_usage(id, session_id, trace_id, user_id, usage_json MEDIUMTEXT, total_tokens INT, created_at TIMESTAMP(3))`，FK `session_id → sessions ON DELETE CASCADE`，`KEY (session_id, created_at)`。`usage_json` 存 done 事件的完整 usage 对象（含 `total` 与 `by_agent`）。在 `SessionService.SaveMessagesBatch` 末尾同事务追加一次 `turn_usage` 插入。
**Rationale:** 独立表保持消息日志干净（done 事件当初正是为此被排除出 messages）。`trace_id` 对应单 turn，支持"某次请求花了多少"。冗余 `total_tokens` 列便于不解析 JSON 做 per-session 聚合查询。同事务保证消息与成本原子一致。
**Alternatives considered:**
- *存为 `event_type='usage'` 的 message 行*：污染消息日志，违背 done 事件排除初衷，且 RecoverMessages 重建对话历史时要过滤。弃。
- *存到 Redis/FS 缓存层*：usage 无热读路径，无需缓存。弃。

### D3: structured output 仅用于子 agent 终轮，model-gated
**Choice:** `AgentConfig` 新增 `output_schema`（可选 raw JSON）。当配置存在：子 agent 的 tool-calling 循环中，仅当模型已无 tool_call（即进入终轮、`finish_reason=stop`）时，对该轮 `LLMRequest` 追加 `response_format: {type:"json_schema", json_schema:{...}}`。`thinking: true`（reasoning 模型）时跳过 structured output（reasoning 模型与 structured output 交互受限），降级为纯 prompt 约束（系统 prompt 要求返回该 schema 的 JSON）。
**Rationale:** 结构化收益集中在"最终交付给父级的综合结论"。中间轮（带 tool_call）不应强制结构化——模型边调工具边被约束 JSON 会冲突。终轮强制保证父级拿到合规 JSON。reasoning 模型的 model-gate 避免已知约束冲突。Liang 优先（分析型结构化收益高），Chongzhi 不配（文件改动本质非文本结构）。
**Alternatives considered:**
- *全程 structured output*：与中间轮 tool_call 冲突。弃。
- *纯 prompt 约束（无 API 强制）*：最轻但模型不保证合规，格式漂移。作为 reasoning 模型降级路径保留。

### D4: 重试 per-agent 幂等感知，分类驱动，配预算
**Choice:**
- 错误分类映射到可重试性：LLM `429/5xx/超时` → 可重试（指数退避，≤2 次）；`tool_error` 瞬时 → 可重试（≤1 次）；`bad_args`/`unknown_tool` → 永不重试。
- **幂等判定**：Liang（只读，tools 全为 webfetch/MCP 读）默认可重试。Chongzhi（xizhi 写工具）**仅当本次 `Run` 尚未触发任何 `dispatchOneRegistryTool` 成功调用时**可重试——即 LLM 调用本身失败、尚未触碰文件系统。一旦有任何 tool_call 已执行（哪怕只是 read），视为已产生状态，不可重试。
- 重试预算：每 turn 累计重试 token 上限（沿用 turn_cost_tracking 数据，超限则停止重试并将错误喂回模型）。
- 重试时复用相同 tool_call args；发射 `agent_error` 带 `Meta.retry=true` 让前端知晓。
**Rationale:** 无脑重试 Chongzhi 会重复执行文件写副作用——危险。幂等判定是核心安全约束。语义错误重试必再败——浪费。预算防止抽风下游烧光成本。
**Alternatives considered:**
- *喂回模型让其自行重派*：当前行为；可作为 structured error（A 的延伸）的后续增强，但首版用确定性重试覆盖 80% 瞬时场景。

### D5: 并行决策指导放 Confucius 系统 prompt，由 turn_cost_tracking 度量
**Choice:** 在 Confucius 的 `cfg.SystemPrompt`（config.yaml）追加并行指导段落（独立子任务单 turn 多 tool_call；有依赖才串行；预算 2-3、避免 >5；禁重叠任务）。不改动 `prompt.RenderSystemPrompt` 渲染逻辑——纯 base prompt 文本。
**Rationale:** 最便宜（零代码）、模型对显式并行指导高度敏感。但效果不可凭感觉判断——必须用 turn_cost_tracking 的 `by_agent` 与 `meta.parallel`/`sub_agent_invocations` 度量并行命中率与成本。E 与 B 天生耦合。
**Alternatives considered:**
- *代码层强制最小并行度*：过度工程、剥夺模型自主性。弃。

### D6: done 事件 usage 形状扩展（非破坏），前后端协同迁移
**Choice:** done 事件 `Meta.usage` 从扁平 `{prompt_tokens, completion_tokens, total_tokens[, reasoning_tokens]}` 变为：
```
{
  "total": {prompt_tokens, completion_tokens, total_tokens, reasoning_tokens?},
  "by_agent": {"Confucius":{...}, "Chongzhi":{...}, "Liang":{...}},
  "meta": {"sub_agent_invocations": ["invoke_chongzhi",...], "parallel": true|false}
}
```
**Rationale:** 旧扁平字段被移入 `total`——这是破坏性的字段路径变更。鉴于 done 事件仅前端消费、且当前实现已违反 spec（前端实际拿不到 per-agent），选择一次性切净而非双发。需同步更新 `api/openapi.yaml` 与 blowball-frontend。
**Alternatives considered:**
- *双发（保留顶层旧字段 + 新增嵌套）*：过渡期兼容性好，但长期留下歧义字段。鉴于当前实现本就未真正提供 per-agent，采纳净切换 + 前端同步迁移。

## Risks / Trade-offs

- **[done 事件 usage 形状变更破坏旧前端]** → 旧前端读 `usage.total_tokens` 会断。*Mitigation:* 同 PR 更新 `api/openapi.yaml` 与 blowball-frontend；当前实现本就未兑现 per-agent，切换无真实功能回退。
- **[structured output 与 reasoning 模型冲突]** → `thinking:true` 时 structured output 可能报错或降质。*Mitigation:* D3 的 model-gate——`thinking:true` 跳过 API 强制、降级纯 prompt；config 校验拒绝 `thinking:true` 且 `output_schema` 非空时启用强制模式（仅允许降级路径）。
- **[Chongzhi 重试误判幂等]** → 幂等判定基于"是否有 tool_call 已执行"，但 xizhi read 也算 tool_call——若 read 后 LLM 失败，重试会重跑 read（无害但耗 token）。更险的是边界判定 bug 导致写后重试。*Mitigation:* 幂等判定保守化：Chongzhi 默认 `retry.enabled=false`，仅在配置显式开启时生效；测试覆盖"写后失败不重试"场景。
- **[turn_usage 表随会话膨胀]** → 高频会话累积大量行。*Mitigation:* FK cascade 随 session 删除清理；后续可加按 `created_at` 的 TTL 清理（本变不做）。
- **[重试放大成本]** → 抽风下游触发连续重试。*Mitigation:* D4 的重试预算（累计 token 上限），超限停止重试、错误喂回模型。
- **[同事务写 turn_usage 增加 SaveMessagesBatch 失败面]** → turn_usage 写失败可能回滚整批消息。*Mitigation:* turn_usage 写入失败仅记日志、不回滚消息批次（usage 是观测数据，消息是业务数据，优先级不同）——与现有"MySQL 错误吞掉不阻塞 SSE"哲学一致。

## Migration Plan

1. **数据库**：新增 `migrations/010_turn_usage.sql`（建 `turn_usage` 表）。docker compose 首次初始化自动执行；已有库手动 `mysql < 010_turn_usage.sql`。无数据回填（历史无 usage 记录）。
2. **代码（B 优先）**：
   - `internal/agent/confucius.go`：dispatch 循环组装 `byAgent`。
   - `internal/agent/orchestrator.go`：`doneUsage` 加 `byAgent`，`emitDone` 发射新形状。
   - `internal/handler/ports.go`：adapter 从 done 事件提取 usage（不再排除其 metadata，仅排除其作为消息内容）。
   - `internal/model` + `internal/store/mysql`：`TurnUsage` + `SaveTurnUsage`。
   - `internal/service/session.go`：`SaveMessagesBatch` 追加 turn_usage 写入（失败仅日志）。
3. **代码（E）**：config.yaml Confucius 系统 prompt 追加并行指导；config.example.yaml 示例。
4. **代码（A）**：`AgentConfig.OutputSchema`；`openai_client.go` 终轮 `response_format`；`config.go` model-gate 校验。
5. **代码（C）**：`dispatchSubAgent` 重试包装 + 幂等判定 + 分类；`AgentRetryConfig`。
6. **契约**：更新 `api/openapi.yaml` done 事件 schema；拷贝至 blowball-frontend 重新生成。
7. **回滚**：revert 代码；`turn_usage` 表可保留（空表无副作用）或 `DROP TABLE turn_usage`。done 事件形状回退到扁平——前端需同步回退。无数据迁移负担（usage 历史本就为空）。

## Open Questions

- **`Agent.Run` 签名：新增返回值 vs `RunResult` 结构？** 倾向新增一个 `byAgent map[string]Usage` 返回值（叶子 agent 返 nil），改动面小于 `RunResult` 包装。实现时确认对 `confucius_test.go`/`orchestrator_test.go` mock 的影响。Resolve at task 1 of B.
- **turn_usage 是否需要 `008_deletion_archive.sql` 风格的镜像表？** 当前 `008` 为 sessions/titles/messages 建了 `*_deleted`。turn_usage 随 session cascade 删除即丢失成本审计。若运营需要"删会话后仍可查历史成本"，后续补 `turn_usage_deleted`。本变先不做——(decided: defer)。
- **structured output 的 schema 放 config.yaml 内联还是独立文件？** 内联 YAML 多行字符串 vs `output_schema_file: liang.json`。倾向内联（单文件可读），过大时再拆。Resolve at A task.
- **重试退避参数默认值？** 倾向 `max_attempts=2`、`initial_backoff=500ms`、`max_backoff=4s`。实现时用真实 429 场景调参。
- **done 事件 usage 的 `meta.parallel` 如何判定？** 由 Confucius dispatch 循环记录"是否存在一个 turn 内 ≥2 个 tool_call 同时派发"。需在 Run 中加计数。
