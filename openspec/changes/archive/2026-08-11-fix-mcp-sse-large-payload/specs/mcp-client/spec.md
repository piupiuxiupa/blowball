## ADDED Requirements

### Requirement: Large message handling for SSE and stdio transports
SSE 与 stdio 传输 SHALL 在读取换行分隔的 JSON-RPC 消息时不以固定单行长度为上限截断；单条消息 SHALL 被完整读取。仅当单条消息的字节数超过显式上限时，系统 SHALL 返回清晰的超限错误并终止当前读循环，且 SHALL NOT 以 `bufio.Scanner: token too long` 的形式把扫描器内部错误暴露给调用方。

#### Scenario: Large JSON-RPC message over the prior 1 MiB scanner cap
- **WHEN** SSE 或 stdio MCP server 以单行返回一个超过 1 MiB 但未超过显式上限的 JSON-RPC 消息
- **THEN** 系统完整读取该消息，不返回 token-too-long 类错误

#### Scenario: Oversized message yields a clear error
- **WHEN** SSE 或 stdio MCP server 返回的单条 JSON-RPC 消息字节数超过显式上限
- **THEN** 系统 DID NOT 静默截断，且返回一个明确的超限错误（可被对应 transport 的上下文包装定位），而非 `bufio.Scanner: token too long`
