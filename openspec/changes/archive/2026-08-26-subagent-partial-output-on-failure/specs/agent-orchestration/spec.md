## MODIFIED Requirements

### Requirement: Agent as tool via function calling
Confucius SHALL 通过 OpenAI function-calling 机制调度子 Agent，每个子 Agent 定义为一个 tool。

子 Agent 执行失败且被放弃（不重试、重试耗尽或取消）时，返回给 Confucius 的 tool result SHALL 携带「错误文本 + 已积累的部分输出」：子 Agent `Run` 在错误返回值中提供部分输出（失败轮已流出的 assistant 文本，或 continuation 耗尽时跨尝试累积的部分内容）且该内容非空白时，tool result content SHALL 为错误文本与部分输出的拼接（错误文本在前，以固定标记行分隔）；部分输出为空白时，tool result content SHALL 与错误文本字节级一致。失败结果 SHALL 保持 `isError` 语义且不套用 tool-result-envelope。

#### Scenario: Confucius receives function call
- **WHEN** OpenAI 返回包含 tool_calls 的响应，function name 为 invoke_chongzhi 或 invoke_liang
- **THEN** 系统解析 parameters 中的 task 和 context，启动对应子 Agent

#### Scenario: Tool call result returned to Confucius
- **WHEN** 子 Agent 执行完成（成功或失败）
- **THEN** 结果作为 tool role message 追加到 Confucius 的消息列表，Confucius 进行下一轮决策；失败时 content 为「错误文本 + 已积累部分输出」的合并文本

#### Scenario: Failed sub-agent carries partial output
- **WHEN** 子 Agent Run 中途 LLM 调用失败（如 llm_error、length_exhausted）且失败前已有 assistant 文本流出
- **THEN** 放弃后返回 Confucius 的 tool result content 由错误文本、固定分隔标记 `--- partial output before failure ---` 与部分输出组成，且 `isError` 为 true

#### Scenario: Blank partial output keeps legacy error text
- **WHEN** 子 Agent 失败且 Run 返回的部分输出为空白（如 round_cap_exhausted 无可抢救内容）
- **THEN** tool result content 与错误文本字节级一致（零行为回归）

#### Scenario: Retry give-up uses the last attempt's partial output
- **WHEN** 子 Agent 首次失败后经历重试，最终重试耗尽放弃
- **THEN** tool result 携带的是最后一次尝试的部分输出；中间失败尝试的 `agent_error`（Meta.retry=true）事件不携带部分输出

#### Scenario: Partial output visible across turn boundary
- **WHEN** 失败子 agent 调用的 tool_result 事件被持久化，后续 turn 重建历史
- **THEN** Confucius 上下文中的 invoke tool_call/tool_result 对携带同一合并文本；子 agent 名下的事件行不参与历史重建（既有约定不变）
