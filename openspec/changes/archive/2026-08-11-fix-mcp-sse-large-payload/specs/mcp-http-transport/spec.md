## ADDED Requirements

### Requirement: Large SSE response handling
Streamable HTTP transport SHALL 在读取 SSE 响应时不以固定单行长度为上限截断消息；承载整条 JSON-RPC 响应的单条 `data:` 消息 SHALL 被完整读取。仅当单条消息的字节数超过显式上限时，系统 SHALL 返回清晰的超限错误，且 SHALL NOT 以 `bufio.Scanner: token too long` 的形式把扫描器内部错误暴露给调用方。

#### Scenario: tools/call result larger than the prior 1 MiB scanner cap
- **WHEN** HTTP MCP server 以单条 `data:` 行返回一个超过 1 MiB 但未超过显式上限的 `tools/call` 成功结果
- **THEN** 系统完整读取该响应并将结果返回给 agent，不返回 token-too-long 类错误

#### Scenario: Oversized SSE response yields a clear error
- **WHEN** HTTP MCP server 返回的单条 `data:` 消息字节数超过显式上限
- **THEN** 系统 DID NOT 静默截断，且返回一个明确的超限错误（可被 `http tools/call:` 上下文包装定位），而非 `bufio.Scanner: token too long`
