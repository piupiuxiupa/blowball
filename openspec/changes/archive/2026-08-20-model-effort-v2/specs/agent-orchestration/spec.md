# agent-orchestration 变更规格

## MODIFIED Requirements

### Requirement: Agent configuration from file

每个 Agent 的 name、system_prompt、max_tokens、tools 列表、mcp 配置、skills 配置、output_schema 及 max_rounds（tool-calling 循环上限）SHALL 从 config.yaml 加载，其中 tools 列表中的名称可以解析为内置工具或已通过 MCP client 注册的外部 MCP 代理工具。agent 配置 SHALL NOT 包含模型、思考开关或思考等级字段——模型与思考等级是 turn 级属性，由模型目录 + 部署默认 + 请求参数解析得出（见 per-request-model-selection），并统一注入该 turn 的全部 agent。未设置或 `<= 0` 的 `max_rounds` SHALL 回退到默认值 `100`。

#### Scenario: Load agent config on startup

- **WHEN** 服务启动
- **THEN** 系统从 config.yaml 的 agents 段加载所有 Agent 配置，并从合并后的工具注册表（内置工具 + 外部 MCP 代理工具）解析 tools 列表，构建 Agent 实例

#### Scenario: Configurable tool permissions

- **WHEN** Agent 配置中 tools 列表为空且 mcp.servers 为空
- **THEN** 该 Agent 调用 LLM 时不传递 tools 参数

#### Scenario: Configurable MCP permissions

- **WHEN** Agent 配置中 mcp.servers 非空
- **THEN** 系统仅把允许的服务器及工具纳入该 Agent 的工具列表和系统提示词

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

- **WHEN** 一个运行在 reasoning wire 家族的 Agent（该 turn 解析到 `thinking: true` 的目录条目）触达上限并执行收尾回合
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

### Requirement: Structured sub-agent return contract

配置了 `output_schema` 的子 agent SHALL 在其 tool-calling 循环的终轮（模型不再 emit tool_call、即将返回最终内容给父级时）启用 OpenAI structured output（`response_format: json_schema`），使返回给父级的内容符合声明 schema。未配置 `output_schema` 的子 agent SHALL 返回自由文本。结构化输出与 reasoning 互斥，该冲突 SHALL 在到达 LLM 之前被拦截：部署默认等级 ≠ none 时配置加载失败（见 agent-reasoning-configuration）；请求解析结果 effort ≠ none 时返回 400（见 per-request-model-selection）。

#### Scenario: Sub-agent with output schema uses structured output on final round

- **WHEN** 一个配置了 `output_schema` 的子 agent（如 Liang）在其循环中进入终轮（`finish_reason=stop`，无 tool_call）
- **THEN** 该终轮的 LLM 请求携带 `response_format: json_schema`（内容为配置的 schema）
- **AND THEN** 子 agent 返回给父级的内容（toolResult.content）是该 schema 的合规 JSON

#### Scenario: Intermediate tool rounds not forced to structured output

- **WHEN** 配置了 `output_schema` 的子 agent 在中间轮仍需调用工具（emit tool_call）
- **THEN** 这些中间轮的 LLM 请求不携带 `response_format`，避免与 tool_call 冲突

#### Scenario: Reasoning turn never reaches a structured-output agent

- **WHEN** 某配置了 `output_schema` 的 agent 存在,且部署默认等级为非 `none`,或请求解析结果 effort ≠ none
- **THEN** 前者使配置加载失败、后者返回 HTTP 400,任何 `output_schema` agent 的 LLM 调用都不会以 effort ≠ none 执行

#### Scenario: Sub-agent without output schema returns free text

- **WHEN** 一个未配置 `output_schema` 的子 agent（如 Chongzhi）完成执行
- **THEN** 其返回内容为自由文本，行为与变更前一致

#### Scenario: Parent informed of structured return shape

- **WHEN** Confucius 的工具列表中包含一个会返回结构化 JSON 的子 agent invoke 工具
- **THEN** Confucius 的系统 prompt 声明该子 agent 返回结构化 JSON，辅助其综合质量
