## MODIFIED Requirements

### Requirement: Independent agent context
子 Agent SHALL 在独立上下文中运行，并只接收父 Agent 传递的 self-contained task description 与 context。每次派发 SHALL 创建新的隔离实例；系统 SHALL NOT 提供模型驱动的子 Agent 实例续跑机制。

#### Scenario: Sub-agent receives isolated context
- **WHEN** Confucius 派发一个子 Agent
- **THEN** 子 Agent 的消息列表仅包含：自身 system_prompt + 一条 user message（内容为 task + context），不包含用户的完整历史对话

#### Scenario: Additional work uses a fresh dispatch
- **WHEN** 先前子 Agent 的结果不足，需要补充执行
- **THEN** Confucius 发起一个新的 `spawn_subagent` 调用，并在 task/context 中显式携带必要的先前结果；系统不加载旧实例上下文
