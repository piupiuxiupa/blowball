## MODIFIED Requirements

### Requirement: Central timeout enforcement at the registry entry point
The execution timeout SHALL be enforced centrally at the registry's tool-dispatch entry point: before invoking a tool's `Execute`, the dispatcher SHALL derive a child context bounded by the configured duration for that tool name (when one is configured) and pass that context to the tool. Enforcement SHALL cover every tool that flows through the registry, including all three agents (Confucius, Chongzhi, Liang) and MCP proxy tools. A registered built-in tool SHALL NOT discard or replace that dispatch context with an unbounded context on a path that performs the tool's work.

#### Scenario: Timeout cancels a long-running tool
- **WHEN** a tool configured with a 5s timeout runs longer than 5s
- **THEN** its context is cancelled and the tool's execution surfaces a deadline-exceeded error

#### Scenario: Enforcement applies to local tools without a native timeout
- **WHEN** `xizhi_grep` is configured with a timeout and executes a pathological scan
- **THEN** the central timeout bounds it, even though `xizhi_grep` has no native timeout of its own

#### Scenario: Registered xizhi grep receives the dispatch deadline
- **WHEN** `tools.timeouts.xizhi_grep` is configured and an agent invokes the registered `xizhi_grep` tool
- **THEN** argument parsing, search-engine execution, and context-line assembly observe the same registry-derived deadline
- **AND** work that remains in progress when the deadline fires surfaces a deadline-exceeded failure rather than continuing as an unbounded call
