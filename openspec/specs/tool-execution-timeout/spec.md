# tool-execution-timeout Specification

## Purpose

Per-tool execution timeout enforced centrally at the registry dispatch entry point, opt-in via the `tools.timeouts` map.

## Requirements

### Requirement: Per-tool execution timeout configuration
The system SHALL accept an operator-configured map `tools.timeouts` mapping built-in tool names (e.g. `xizhi_grep`, `webfetch`, `bash`) to a duration. Each entry bounds the execution time of the named tool when it is dispatched. Tool names SHALL use the same identifiers agents reference in their `tools:` configuration lists.

#### Scenario: Configure a per-tool timeout
- **WHEN** `config.yaml` contains `tools.timeouts: {xizhi_grep: 30s, xizhi_tree: 15s}`
- **THEN** `xizhi_grep` is bounded to 30s and `xizhi_tree` to 15s per invocation

#### Scenario: Duration short-suffix parsing
- **WHEN** a timeout value uses a short suffix (e.g. `30s`, `2m`)
- **THEN** the system parses it via the existing config duration expansion

### Requirement: Central timeout enforcement at the registry entry point
The execution timeout SHALL be enforced centrally at the registry's tool-dispatch entry point: before invoking a tool's `Execute`, the dispatcher SHALL derive a child context bounded by the configured duration for that tool name (when one is configured) and pass that context to the tool. Enforcement SHALL cover every tool that flows through the registry, including all three agents (Confucius, Chongzhi, Liang) and MCP proxy tools.

#### Scenario: Timeout cancels a long-running tool
- **WHEN** a tool configured with a 5s timeout runs longer than 5s
- **THEN** its context is cancelled and the tool's execution surfaces a deadline-exceeded error

#### Scenario: Enforcement applies to local tools without a native timeout
- **WHEN** `xizhi_grep` is configured with a timeout and executes a pathological scan
- **THEN** the central timeout bounds it, even though `xizhi_grep` has no native timeout of its own

### Requirement: Unmapped tools retain current behavior
A tool name that is absent from `tools.timeouts`, or whose configured duration is zero, SHALL incur no execution timeout — preserving the prior behavior byte-for-byte. The feature is opt-in per tool; no global default timeout is applied.

#### Scenario: Unmapped tool is unbounded
- **WHEN** a tool is not listed in `tools.timeouts`
- **THEN** no child timeout context is derived for it and its execution is unbounded (as before)

#### Scenario: Zero duration means no timeout
- **WHEN** a tool is listed with duration `0s`
- **THEN** the system treats it as unbounded

### Requirement: Timeout failure surfaces through the status envelope
A tool execution that exceeds its configured timeout SHALL surface as a failure through the tool-result envelope: the model receives `{"status":1,"error":"<message>"}` describing the timeout. The timeout error flows through the same rendering path as any other tool error.

#### Scenario: Timed-out tool returns failure envelope
- **WHEN** a tool exceeds its `tools.timeouts` budget
- **THEN** the `role="tool"` message body is `{"status":1,"error":...}` indicating the timeout

### Requirement: Central timeout composes with native tool timeouts
For tools that already possess a native timeout (`webfetch` HTTP timeout, per-user `mcp` call timeout, `bash` command timeout), the central execution timeout SHALL act as a looser outer backstop: the native (tighter) bound continues to fire first in normal operation. The central timeout SHALL NOT remove or weaken those native timeouts.

#### Scenario: Native timeout still applies
- **WHEN** `webfetch` is configured with both a native HTTP timeout of 10s and a central `tools.timeouts` entry of 30s
- **THEN** a request exceeding 10s still fails at the native HTTP timeout (the tighter bound)
