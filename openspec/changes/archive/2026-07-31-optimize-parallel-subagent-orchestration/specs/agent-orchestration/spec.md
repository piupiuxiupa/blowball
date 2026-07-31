## MODIFIED Requirements

### Requirement: Token usage observability
系统 SHALL 在每次请求的 done 事件中发射 per-agent token 用量拆分，并按 `turn-cost-tracking` 能力持久化。done 事件 `Meta.usage` SHALL 采用嵌套形状 `{total, by_agent, meta}`，其中 `by_agent` 按每个参与 agent（Confucius 及其调度的子 agent）拆分 prompt_tokens/completion_tokens/total_tokens/reasoning_tokens。

#### Scenario: Done event carries per-agent breakdown
- **WHEN** 一次完整的用户请求处理完成（成功或失败路径）
- **THEN** StreamEvent{Type: "done"} 的 `Meta.usage` 包含 `total`（聚合）与 `by_agent`（per-agent 明细，键为 agent 名）两段
- **AND THEN** `by_agent` 至少包含 `Confucius`；若调度了子 agent，则同时包含对应子 agent 键

#### Scenario: Parallel sub-agent usage attributed separately
- **WHEN** 一个 turn 中 Confucius 并行调度了 Chongzhi 与 Liang
- **THEN** `usage.by_agent` 分别记录 Confucius、Chongzhi、Liang 三者的 token 消耗
- **AND THEN** 三者之和等于 `usage.total`

#### Scenario: Usage persisted per turn
- **WHEN** done 事件发射后，turn 的消息批次被持久化
- **THEN** 系统按 `turn-cost-tracking` 能力将同一 usage 对象写入 `turn_usage` 表（见 turn-cost-tracking spec）

#### Scenario: Error turn still reports usage
- **WHEN** orchestrator 处理失败（非客户端取消）
- **THEN** done 事件仍发射已累积的 usage（含 `total` 与已执行 agent 的 `by_agent`），`Meta.usage` 可选包含 `error` 字段描述失败原因

## ADDED Requirements

### Requirement: Per-agent usage attribution in sub-agent dispatch
Confucius 的 dispatch 循环 SHALL 在折叠子 agent usage 进 turn 总量的同时，保留 per-agent 拆分并沿调用链传递至 done 事件，修复当前实现将子 agent usage 压扁进父总量、导致 per-agent 拆分丢失的缺陷。

#### Scenario: Sub-agent usage preserved separately
- **WHEN** Confucius 通过 `dispatchSubAgent` 执行一个子 agent 并获得其 `subUsage`
- **THEN** 该子 agent 的 usage 同时被加入 per-agent 拆分 map（键为子 agent 名）与 turn 总量，而非仅折入总量

#### Scenario: Confucius own usage attributed to Confucius
- **WHEN** Confucius 自身的 LLM 调用产生 usage
- **THEN** 该 usage 归入 per-agent 拆分的 `Confucius` 键，与子 agent usage 分离

### Requirement: Structured sub-agent return contract
配置了 `output_schema` 的子 agent SHALL 在其 tool-calling 循环的终轮（模型不再 emit tool_call、即将返回最终内容给父级时）启用 OpenAI structured output（`response_format: json_schema`），使返回给父级的内容符合声明 schema。未配置 `output_schema` 的子 agent SHALL 返回自由文本。

#### Scenario: Sub-agent with output schema uses structured output on final round
- **WHEN** 一个配置了 `output_schema` 的子 agent（如 Liang）在其循环中进入终轮（`finish_reason=stop`，无 tool_call）
- **THEN** 该终轮的 LLM 请求携带 `response_format: json_schema`（内容为配置的 schema）
- **AND THEN** 子 agent 返回给父级的内容（toolResult.content）是该 schema 的合规 JSON

#### Scenario: Intermediate tool rounds not forced to structured output
- **WHEN** 配置了 `output_schema` 的子 agent 在中间轮仍需调用工具（emit tool_call）
- **THEN** 这些中间轮的 LLM 请求不携带 `response_format`，避免与 tool_call 冲突

#### Scenario: Reasoning model degrades to prompt-only constraint
- **WHEN** 配置了 `output_schema` 的子 agent 同时启用 `thinking: true`（reasoning 模型）
- **THEN** 系统跳过 API 强制的 `response_format`，改为在系统 prompt 中以文本约束要求返回该 schema 的 JSON（model-gate 降级）
- **AND THEN** 启动校验拒绝 `thinking:true` 且要求强制 structured output 的矛盾配置

#### Scenario: Sub-agent without output schema returns free text
- **WHEN** 一个未配置 `output_schema` 的子 agent（如 Chongzhi）完成执行
- **THEN** 其返回内容为自由文本，行为与变更前一致

#### Scenario: Parent informed of structured return shape
- **WHEN** Confucius 的工具列表中包含一个会返回结构化 JSON 的子 agent invoke 工具
- **THEN** Confucius 的系统 prompt 声明该子 agent 返回结构化 JSON，辅助其综合质量

### Requirement: Transient error retry for sub-agent dispatch
系统 SHALL 对子 agent dispatch 的瞬时错误（LLM 429/5xx/超时、瞬时 tool_error）执行受限重试，并对语义错误（`bad_args`、`unknown_tool`）永不重试。重试策略 SHALL 按 agent 幂等性区分：只读子 agent（Liang）默认可重试；有副作用的子 agent（Chongzhi）仅当本次执行尚未触发任何成功 tool_call 时可重试。

#### Scenario: Transient LLM error retried for read-only agent
- **WHEN** Liang 的 LLM 调用返回 429 或超时
- **THEN** 系统按指数退避重试（至多配置的 max_attempts，默认 2），复用相同 invoke 参数
- **AND THEN** 重试时发射 `agent_error` 事件且 `Meta.retry=true` 以通知前端

#### Scenario: Semantic error never retried
- **WHEN** 子 agent dispatch 因 `bad_args` 或 `unknown_tool` 失败
- **THEN** 系统立即返回错误结果，不重试（相同参数必再败）

#### Scenario: Side-effecting agent not retried after tool execution
- **WHEN** Chongzhi 已成功执行至少一个 xizhi 工具调用（如 write_file）后，其后续 LLM 调用失败
- **THEN** 系统不重试（已产生文件系统副作用，重试将重复执行）
- **AND THEN** 错误结果喂回 Confucius 决策

#### Scenario: Side-effecting agent retried only before any tool call
- **WHEN** Chongzhi 首轮 LLM 调用即失败（尚未触发任何 tool_call）
- **THEN** 系统可重试该 LLM 调用（无副作用风险），重试上限受重试预算约束

#### Scenario: Retry budget enforced
- **WHEN** 一个 turn 内累计重试消耗的 token 达到配置预算上限
- **THEN** 系统停止后续重试，将当前错误结果喂回 Confucius（依赖 `turn-cost-tracking` 数据防失控）

#### Scenario: Retry exhausted surfaces error to parent
- **WHEN** 重试次数耗尽仍失败
- **THEN** 系统将最终错误作为 tool result 喂回 Confucius，由其决定后续（重派/降级/告知用户）

### Requirement: Parallel dispatch decision guidance in system prompt
Confucius 的系统 prompt SHALL 包含并行调度决策指导，使模型在独立子任务时于单个 assistant turn 内 emit 多个 tool_calls，在有依赖时串行，并遵守并行预算。

#### Scenario: Guidance present in Confucius prompt
- **WHEN** Confucius 的系统 prompt 被构建
- **THEN** prompt 文本包含并行决策指导：独立子任务单 turn 并行；有依赖才串行；并行预算 2-3、避免 >5；禁止对同一子 agent 发重叠任务

#### Scenario: Sequential dispatch when dependency exists
- **WHEN** 任务 B 的输入依赖任务 A 的输出
- **THEN** 指导文本明确要求模型先完成 A、拿到结果后再发起 B（不在同一 turn 并行 emit）
