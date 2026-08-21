# agent-orchestration 变更规格

## MODIFIED Requirements

### Requirement: Confucius agent loop
Confucius SHALL 实现多轮 tool-calling 循环，循环在 LLM 返回 finish_reason 为 stop 时自然终止；当循环用尽配置的 `max_rounds` 上限仍未自然终止时，SHALL 按「Agent tool-calling loop round cap and graceful termination」受控终止（WARN 日志、`usage.meta.round_capped`、一次 tool-disabled 收尾回合；详见该需求）。当某回合返回 `finish_reason=length` 时，SHALL 按 `llm-length-continuation` 能力处理：能力启用时不再将 `length` 视为循环终止信号，而是保留已输出内容并以扩容预算续写（续写尝试不消耗 `max_rounds`；耗尽时的 `length_exhausted` 终止见该能力）；能力未启用时维持 `length` 静默终止的现状行为。同一续写规则 SHALL 等价作用于 Chongzhi 与 Liang 的 tool-calling 循环及各 agent 的 tool-disabled 收尾回合。

#### Scenario: Confucius calls tools then summarizes
- **WHEN** Confucius 首轮调用返回 tool_calls，执行后第二轮 LLM 返回 content 且 finish_reason 为 stop
- **THEN** Confucius 输出最终汇总内容，推送 done 事件

#### Scenario: Confucius handles directly
- **WHEN** Confucius 首轮调用直接返回 content 且 finish_reason 为 stop（无 tool_calls）
- **THEN** Confucius 直接输出内容，推送 done 事件

#### Scenario: length round continues instead of terminating
- **WHEN** `openai.length_continue` 已启用，Confucius（或 Chongzhi/Liang）某回合返回 `finish_reason=length`
- **THEN** 该回合按 `llm-length-continuation` 能力续写（保留已输出内容、扩容预算重发），不终止循环、不消耗 `max_rounds`

#### Scenario: length still terminates when capability disabled
- **WHEN** `openai.length_continue` 未启用，某回合返回 `finish_reason=length`
- **THEN** 循环按改动前行为终止（含 `length` 携带 tool_calls 时 `shouldDispatchToolCalls` 的 WARN + terminal 处理），不追加任何续写脚手架
