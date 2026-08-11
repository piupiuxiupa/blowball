## MODIFIED Requirements

### Requirement: Agent configuration from file
每个 Agent 的 system_prompt、model、max_tokens、tools 列表、mcp 配置、skills 配置、thinking 开关、reasoning_effort 配置及 max_rounds（tool-calling 循环上限）SHALL 从 config.yaml 加载，其中 tools 列表中的名称可以解析为内置工具或已通过 MCP client 注册的外部 MCP 代理工具。未设置或 `<= 0` 的 `max_rounds` SHALL 回退到默认值 `100`。

#### Scenario: Load agent config on startup
- **WHEN** 服务启动
- **THEN** 系统从 config.yaml 的 agents 段加载所有 Agent 配置，并从合并后的工具注册表（内置工具 + 外部 MCP 代理工具）解析 tools 列表，构建 Agent 实例

#### Scenario: Configurable tool permissions
- **WHEN** Agent 配置中 tools 列表为空且 mcp.servers 为空
- **THEN** 该 Agent 调用 OpenAI 时不传递 tools 参数

#### Scenario: Configurable MCP permissions
- **WHEN** Agent 配置中 mcp.servers 非空
- **THEN** 系统仅把允许的服务器及工具纳入该 Agent 的工具列表和系统提示词

#### Scenario: Configurable skill permissions
- **WHEN** Agent 配置中 skills 列表非空
- **THEN** 系统仅把这些 skill 纳入该 Agent 的系统提示词 skill catalog

#### Scenario: Default max_rounds when unset
- **WHEN** 某 Agent 配置未设置 `max_rounds`（或设为 `<= 0`）
- **THEN** 该 Agent 的 tool-calling 循环上限 SHALL 取默认值 `100`

### Requirement: Confucius agent loop
Confucius SHALL 实现多轮 tool-calling 循环，循环在 LLM 返回 finish_reason 为 stop 时自然终止；当循环用尽配置的 `max_rounds` 上限仍未自然终止时，SHALL 按「Agent tool-calling loop round cap and graceful termination」受控终止（WARN 日志、`usage.meta.round_capped`、一次 tool-disabled 收尾回合；详见该需求）。

#### Scenario: Confucius calls tools then summarizes
- **WHEN** Confucius 首轮调用返回 tool_calls，执行后第二轮 LLM 返回 content 且 finish_reason 为 stop
- **THEN** Confucius 输出最终汇总内容，推送 done 事件

#### Scenario: Confucius handles directly
- **WHEN** Confucius 首轮调用直接返回 content 且 finish_reason 为 stop（无 tool_calls）
- **THEN** Confucius 直接输出内容，推送 done 事件

## ADDED Requirements

### Requirement: Agent tool-calling loop round cap and graceful termination
每个 Agent（Confucius、Chongzhi、Liang）的 tool-calling 循环 SHALL 受该 Agent 配置的 `max_rounds` 上限约束。当循环用尽 `max_rounds` 轮仍未自然终止（模型持续 emit tool_calls）时，系统 SHALL：(a) 发射一条结构化 WARN 日志（含 agent 名、上限值、已执行轮数）；(b) 将 `usage.meta.round_capped` 置为 `true` 折入终态 done 事件（非 cap 终止的 turn SHALL 不携带该键）；(c) 执行一次"收尾回合"——发起一轮额外 LLM 请求且禁用 tools（Confucius 同时不再 dispatch 子 agent），并在消息末尾追加一条收尾指令（告知模型已达轮数上限、不得再调用工具、须基于既有对话给出最终答复），以引导模型综合而非继续 tool-use 流；该轮 token SHALL 正常计入用量。若收尾回合返回 finish_reason=stop 的可用文本内容（无 tool_calls），其内容成为 turn 的 `finalContent`，turn 按正常成功结束（WARN 日志与 `meta.round_capped` 为仅有的 cap 信号，不发射 `agent_error`，done 事件不携带 `error`）。若收尾回合返回 tool_calls（某些 OpenAI 兼容网关在 tools 缺省时仍回显习得的 tool-use，这些调用无法被受理、其伴随文本仅为"接下来调用 X"式前言而非综合答案）、或未产出内容、或出错，均视为收尾失败：系统 SHALL 发射 `agent_error`（`Meta.error_code` 为 `round_cap_exhausted`）随后 `agent_end`，done 事件 SHALL 携带 `error` 字段，且该前言文本 SHALL NOT 作为最终答案返回。自然终止（`finish_reason=stop`、空 assistant 响应）SHALL 不触发上述任何 cap 行为。

#### Scenario: Cap hit with successful wrap-up round
- **WHEN** 某 Agent 的循环用尽 `max_rounds` 轮仍未自然终止，且随后的 tool-disabled 收尾回合返回了非空 content
- **THEN** 该 content 成为 turn 的最终输出，done 事件的 `usage.meta.round_capped` 为 `true`
- **AND THEN** 系统不发射 `agent_error`，done 事件不携带 `error` 字段

#### Scenario: Cap hit with empty wrap-up round surfaces error
- **WHEN** 某 Agent 的循环用尽 `max_rounds` 轮仍未自然终止，且随后的收尾回合未返回可用内容（空 content 或 LLM 错误）
- **THEN** 系统发射 `agent_error`（`Meta.error_code` 为 `round_cap_exhausted`）随后 `agent_end`
- **AND THEN** done 事件携带 `error` 字段，`usage.meta.round_capped` 为 `true`

#### Scenario: Cap hit where wrap-up emits tool_calls surfaces error
- **WHEN** 收尾回合的 LLM 响应携带 tool_calls（即便请求未携带 tools，某些 OpenAI 兼容网关仍会回显习得的 tool-use）
- **THEN** 系统 SHALL 视为收尾失败：发射 `agent_error`（`Meta.error_code` 为 `round_cap_exhausted`）随后 `agent_end`，done 事件携带 `error` 字段
- **AND THEN** 该响应的伴随文本 SHALL NOT 作为 turn 的最终内容返回

#### Scenario: Wrap-up round disables tools
- **WHEN** 收尾回合的 LLM 请求被构造
- **THEN** 该请求 SHALL 不携带 `tools`（且 Confucius 在该回合不 dispatch 子 agent），使模型只能返回最终文本

#### Scenario: Wrap-up round counts usage normally
- **WHEN** 收尾回合产生 token 用量
- **THEN** 该用量 SHALL 折入该 Agent 的 per-agent 与 turn 总用量，与正常回合一致

#### Scenario: Reasoning agent wrap-up round
- **WHEN** 一个启用 `thinking: true` 的 Agent 触达上限并执行收尾回合
- **THEN** 收尾回合 SHALL 正常产出 reasoning + content，不发生 response_format 冲突

#### Scenario: Structured-output agent wrap-up round
- **WHEN** 一个配置了 `output_schema` 的子 agent（如 Liang）触达上限并执行收尾回合
- **THEN** 该收尾回合作为终轮 SHALL 携带 `response_format: json_schema`，返回内容为该 schema 的合规 JSON

#### Scenario: Configurable cap respected
- **WHEN** 某 Agent 配置 `max_rounds: 3`，模型连续 3 轮 emit tool_calls
- **THEN** 循环在第 3 轮后进入收尾回合（不再执行第 4 个 tool-calling 轮）

#### Scenario: Default cap applied when unset
- **WHEN** 某 Agent 未配置 `max_rounds`
- **THEN** 循环上限 SHALL 为默认值 `100`

#### Scenario: Natural termination emits no cap signal
- **WHEN** 某 Agent 在用尽 `max_rounds` 之前以 `finish_reason=stop` 或空 assistant 响应自然终止
- **THEN** 系统 SHALL 不发射 WARN cap 日志、不设置 `meta.round_capped`、不发射 `round_cap_exhausted`，done 事件不携带 `error`

#### Scenario: Warn log emitted on every cap hit
- **WHEN** 任一 Agent 的循环因用尽 `max_rounds` 进入收尾路径
- **THEN** 系统 SHALL 发射一条结构化 WARN 日志，包含 agent 名、`max_rounds` 值与已执行轮数
