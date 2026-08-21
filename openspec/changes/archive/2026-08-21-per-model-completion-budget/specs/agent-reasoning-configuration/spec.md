# Delta: agent-reasoning-configuration

## MODIFIED Requirements

### Requirement: Wire family follows catalog entry capability

LLM 请求的 wire 形态 SHALL 由所选目录条目的 `thinking` 能力决定:`thinking: true` 条目 SHALL 在每次请求中发送 `reasoning_effort`(取值包括字面 `none`),并将输出配额(条目的 `max_completion_tokens`)发送为 wire 参数 `max_completion_tokens`(不发送 `max_tokens`);`thinking: false` 条目 SHALL NOT 发送 `reasoning_effort`,并将同一配额发送为 wire 参数 `max_tokens`(不发送 `max_completion_tokens`)。同一 turn 的全部 agent 调用 SHALL 落在同一 wire 家族(条目相同)。配额数值 SHALL 取自该 turn 解析的目录条目,agent 配置 SHALL NOT 携带配额字段。

#### Scenario: none 在思考条目上显式下发

- **WHEN** turn 解析到 `thinking: true` 条目(配额 8192)且 effort 为 `none`
- **THEN** 请求携带 `reasoning_effort="none"` 与 `max_completion_tokens=8192`,不携带 `max_tokens`

#### Scenario: 非思考条目不发送 effort

- **WHEN** turn 解析到 `thinking: false` 条目(配额 8192)
- **THEN** 请求不携带 `reasoning_effort`,携带 `max_tokens=8192`,不携带 `max_completion_tokens`
