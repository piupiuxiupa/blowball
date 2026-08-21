# llm-length-continuation Specification

## Purpose

当一个 LLM 回合的响应因输出 token 上限被截断（`finish_reason == "length"`）时，续写能力（`openai.length_continue`，opt-in）使其被继续而非静默终止：保留已输出内容，向该回合上下文追加续写脚手架后以扩容预算（`max_tokens + N × expand_step`）重发请求，最多 `max_retries` 次续写（尝试不消耗 `max_rounds`）；覆盖 Confucius/Chongzhi/Liang 三个主循环与轮数上限收尾回合（标题生成与 compaction 摘要不覆盖）。SSE 事件面与持久化面零改动——不引入新事件类型、尝试间不插入事件（`MergeEvents` 仍合并为单行）、脚手架不落库。未配置或全零时行为与改动前逐字节一致。

## Requirements

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

### Requirement: 纯 content 截断的续写

`length` 响应不含 tool_calls 时，续写脚手架 SHALL 为：向 `round` 追加 assistant 消息（承载本次尝试的 content/reasoning_content）与一条 user 角色续写指令（告知输出因长度限制中断、从中断处继续、不得重复已写内容、大文件写入应分块多次）。回合最终内容 SHALL 为各次尝试 content 的按序累计。全空响应（thinking 耗尽预算导致的 content 与 tool_calls 皆为空）SHALL 走同一续写路径。

#### Scenario: 截断内容与续写内容累计

- **WHEN** 第 1 次尝试输出 "前半段" 后 `length`，续写尝试输出 "后半段" 后 `stop`
- **THEN** 该回合最终 content 为 "前半段后半段"

#### Scenario: 续写指令进入回合上下文

- **WHEN** 纯 content 截断触发续写
- **THEN** `round` 中追加了一条 user 角色续写指令消息，且该指令在本次 turn 的后续轮次中持续可见

#### Scenario: thinking 耗尽型空响应同样续写

- **WHEN** 第 1 次尝试仅产出 reasoning、content 与 tool_calls 皆为空，`finish_reason=length`
- **THEN** 系统按同一续写路径追加脚手架并以扩容预算重发（为 thinking 腾出空间）

### Requirement: tool_calls 截断的分诊续写

`length` 响应携带 tool_calls 时，系统 SHALL 将含半截 tool_calls 的 assistant 消息追加进 `round`（B-call-1：保留模型自己的部分输出），并按逐 call 分诊：`arguments` 为合法 JSON 的 call SHALL 在续写请求发出前照常 dispatch（真实结果追加进 `round`，正常发射 `tool_call`/`tool_result` 事件）；`arguments` 非合法 JSON（含空串）的 call SHALL 不执行，改追加合成 tool result（内容声明：该调用因输出长度限制参数被截断、未执行、请分块重新发起），并发射一对 SSE 事件——`tool_call` 事件（arguments 净化为 `{}`，避免无效 JSON 破坏事件载荷）与 `tool_result` 事件（承载合成结果文本）。每个 assistant 消息中的 tool_call 在下一请求发出前 SHALL 拥有对应的 tool 角色应答。

#### Scenario: 完整 call 照常执行

- **WHEN** `length` 响应含两个 tool_call，第一个 arguments 为合法 JSON、第二个被截断为非法 JSON
- **THEN** 第一个 call 正常 dispatch 且其真实结果进入 `round`；第二个 call 不执行并获得合成 tool result；续写请求发出前两个 call 均有 tool 角色应答

#### Scenario: 截断 call 发射净化事件对

- **WHEN** 某个被截断的 tool_call 走合成结果路径
- **THEN** 系统发射 `tool_call` 事件（arguments 为 `{}`）与 `tool_result` 事件（合成结果文本），事件流不因此出现非法 JSON 载荷

#### Scenario: 模型在续写中重新发起调用

- **WHEN** 分诊续写后的下一次尝试返回完整的同名 tool_call
- **THEN** 该 call 按 normal dispatch 路径执行（携带新的 tool_call id）

### Requirement: 续写耗尽终止

续写次数 SHALL 以 `max_retries` 为上限（默认 3，即最多 4 次尝试）。最终一次尝试仍为 `finish_reason=length` 时，系统 SHALL 发射 `agent_error`（`Meta.error_code` 为 `length_exhausted`）随后 `agent_end`，并使 turn 以错误结束（done 事件携带 `error` 字段）；`finalContent` SHALL 为各次尝试已累计的内容（续写路径不丢弃任何已输出内容）。

#### Scenario: 耗尽后的事件序列

- **WHEN** 4 次尝试全部 `finish_reason=length`
- **THEN** 系统发射 `agent_error`（`Meta.error_code == "length_exhausted"`）随后 `agent_end`，done 事件携带 `error` 字段

#### Scenario: 耗尽仍保留累计内容

- **WHEN** 续写耗尽终止
- **THEN** turn 的 `finalContent` 为已累计的部分内容，而非空串

### Requirement: 续写用量记账

续写路径的所有尝试（含被 `length` 截断的）产生的用量 SHALL 全部计入 turn 的 `total` 与 `by_agent`。回合的 `context_tokens`（`usage.meta.context_tokens` → `turn_usage.context_tokens`）SHALL 仅记录最终一次尝试的用量（下一次请求的真实大小），MUST NOT 跨尝试累加。

#### Scenario: 全部尝试计入用量

- **WHEN** 一轮共 3 次尝试（2 次 length + 1 次 stop），各产生用量 U1、U2、U3
- **THEN** `total` 与该 agent 的 `by_agent` 条目包含 U1+U2+U3

#### Scenario: context_tokens 只记最终尝试

- **WHEN** 同上场景
- **THEN** 该回合观察到的 `context_tokens` 等于 U3 的 prompt+completion，而非 U1+U2+U3 之和

### Requirement: SSE 与持久化零改动不变量

续写 SHALL NOT 引入新的事件类型或修改现有事件 schema。相邻两次尝试的 token 流之间 SHALL NOT 插入任何事件，使 `MergeEvents` 将同一 `(type, agent, run_id)` 的全部 token 事件合并为单条消息行。续写脚手架（user 续写指令、合成 tool result）SHALL 仅存在于本轮 `round` 上下文，MUST NOT 生成 `message` 类持久化行（跨 turn 重建的上下文形状允许与活 turn 不同，内容等价即可）。

#### Scenario: 多次尝试合并为单行

- **WHEN** 一轮 2 次尝试的 token 事件（同一 agent、同一 run、中间无其他事件）被持久化
- **THEN** `MergeEvents` 将其合并为一条消息行，内容为两次尝试的按序拼接

#### Scenario: 脚手架不落库

- **WHEN** 一轮发生纯 content 截断续写并最终成功，turn 结束后重建历史
- **THEN** 持久化与重建的上下文中不出现续写指令 user 消息行，仅内容等价的 assistant token 行

### Requirement: 续写覆盖范围

续写 SHALL 通过一个共享 helper 覆盖四个 `StreamChat` 调用点：Confucius、Chongzhi、Liang 三个主循环，以及轮数上限收尾回合（`runWrapUpRound`）。TitleService 的标题生成与 compaction 摘要生成 SHALL NOT 触发续写。

#### Scenario: 收尾回合同样续写

- **WHEN** 某 agent 的 tool-disabled 收尾回合返回 `finish_reason=length`（纯 content）
- **THEN** 收尾回合经同一续写路径扩容重发；若最终尝试返回 tool_calls 或空内容，仍走既有 `round_cap_exhausted` 路径

#### Scenario: 标题生成不续写

- **WHEN** 标题生成调用的响应 `finish_reason=length`
- **THEN** 不触发续写，行为与现状一致

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
