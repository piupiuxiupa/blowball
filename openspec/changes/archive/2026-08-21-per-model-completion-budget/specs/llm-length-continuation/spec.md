# Delta: llm-length-continuation

## MODIFIED Requirements

### Requirement: length 续写触发与预算扩容

启用续写能力(该 turn 解析条目配置了非零 `length_continue` 子块)时,受覆盖调用点的一个 LLM 回合响应 `finish_reason == "length"` SHALL 触发续写:保留已输出内容,向该回合上下文(`round`)追加续写脚手架后重发请求,不终止循环。第 N 次尝试(0 基)请求的输出配额 SHALL 为该条目的 `max_completion_tokens + N × 该条目 expand_step`(thinking 家族映射到 `max_completion_tokens` 的现有规则不变)。续写尝试 SHALL NOT 消耗该 agent 的 `max_rounds` 轮数。所选条目未启用续写时 SHALL 保持现状行为逐字节不变(`length` 静默终止,`shouldDispatchToolCalls` 对 length+tool_calls 的 WARN + terminal 处理不变)。

#### Scenario: length 触发续写后正常结束

- **WHEN** 某 agent 回合第 1 次尝试返回 `finish_reason=length`(有 content),第 2 次尝试(扩容预算)返回 `finish_reason=stop`
- **THEN** 循环不终止于第 1 次尝试,最终内容为两次尝试 content 的累计

#### Scenario: 每次尝试的输出预算递增

- **WHEN** 一轮发生 2 次续写(共 3 次尝试),turn 解析条目配置 `max_completion_tokens: 8192`、`expand_step: 8192`
- **THEN** 三次请求的输出预算分别为 8192、16384、24576(按 `llm_raw_log` 的 `kind=request` 行可逐一验证)

#### Scenario: 续写尝试不消耗 max_rounds

- **WHEN** 某 agent 配置 `max_rounds: 3`,其中一轮发生了 2 次续写
- **THEN** 该轮仍计为 1 轮,循环的轮数上限判定不受续写影响

#### Scenario: 所选条目未启用时行为不变

- **WHEN** turn 解析条目未配置 `length_continue`(或全零),某回合返回 `finish_reason=length`
- **THEN** 行为与启用前逐字节一致:不追加脚手架、不重发、循环按现状终止

#### Scenario: 同一部署内续写按条目区分

- **WHEN** 目录条目 A 配置了 `length_continue`,条目 B 未配置,两 turn 分别选择 A 与 B 且均遇到 `finish_reason=length`
- **THEN** 选择 A 的 turn 触发续写,选择 B 的 turn 按未启用行为静默终止

### Requirement: 续写配置

系统 SHALL 支持逐目录条目配置子块 `openai.models[].length_continue`,含 `expand_step`(int,缺省 8192)与 `max_retries`(int,缺省 3)。条目未配置该子块或全零 SHALL 对该条目禁用续写;任一字段非零即对该条目启用,未设置的兄弟字段取缺省。负值或非整数值 SHALL 使配置加载失败。顶层全局块 `openai.length_continue` SHALL 被拒绝(见 per-request-model-selection 的残留拒绝需求)。

#### Scenario: 缺省禁用

- **WHEN** 某 目录条目未配置 `length_continue`
- **THEN** 该条目的续写能力禁用,`length` 行为与改动前一致

#### Scenario: 部分配置补默认

- **WHEN** 某条目配置 `length_continue: { expand_step: 4096 }`
- **THEN** 该条目续写启用且 `max_retries` 取缺省 3

#### Scenario: 非法值拒绝加载

- **WHEN** 某条目 `length_continue` 的 `expand_step` 或 `max_retries` 为负数
- **THEN** 配置加载失败,服务不启动

#### Scenario: 条目间配置互不串扰

- **WHEN** 条目 A 配置 `length_continue: {expand_step: 4096, max_retries: 1}`,条目 B 配置 `length_continue: {expand_step: 16384}`
- **THEN** 选择 A 的 turn 扩容步长 4096、最多 1 次续写;选择 B 的 turn 扩容步长 16384、最多 3 次续写(缺省)
