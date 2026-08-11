## Why

MCP 客户端读取 SSE / 换行分隔的 JSON-RPC 响应时使用 `bufio.Scanner`，并把单行 buffer 封顶在 `1024*1024`（1 MiB）。MCP Streamable HTTP 协议会把整个 JSON-RPC 响应放在一条 SSE `data:` 行里，SSE/stdio 传输同样以换行分隔单条消息。当某次 `tools/call` 的返回体（大文件读取、base64 编码的二进制/图片、超大查询结果等）的单行超过 1 MiB 时，scanner 返回 `bufio.ErrTooLong`，对外表现为含义不明的 `http tools/call: read sse data: bufio.Scanner: token too long`，导致返回体较大的 MCP 工具调用间歇性失败（小返回体正常，大返回体必现）。

## What Changes

- 用无单行长度上限的读取方式（`bufio.Reader` + `ReadString('\n')` / `ReadBytes('\n')`）替换 `internal/tool/mcpclient` 下 HTTP `readSSEData`、SSE `readLoop`、stdio `readLoop` 中的 `bufio.Scanner`，根除 `token too long`。
- 引入一个显式、较大的响应/消息软上限（`io.LimitReader` 或等价方式），仅在超过该上限时返回清晰的 "response/message too large" 错误，替代当前被 scanner 截断后含义不明的错误。
- 补充测试：以单行 > 1 MiB 的 `data:` / JSON 消息验证三条传输路径均能正确读取大返回体，且超过上限时返回明确错误。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `mcp-http-transport`: 新增需求 —— Streamable HTTP 的 SSE 响应读取不受固定单行长度限制，大返回体应被完整读取；仅在超过显式上限时返回清晰错误，而非 `token too long`。
- `mcp-client`: 新增需求 —— SSE 与 stdio 传输的消息读取不受固定单行长度限制，大消息应被完整读取；仅在超过显式上限时返回清晰错误。

## Impact

- 代码：`internal/tool/mcpclient/http.go`（`readSSEData`）、`internal/tool/mcpclient/sse.go`（`readLoop`）、`internal/tool/mcpclient/stdio.go`（`readLoop`）。
- 测试：`internal/tool/mcpclient/http_test.go`、`sse_test.go`、`stdio_test.go`（及对应 testdata）。
- 不涉及 HTTP API 契约、配置项语义、持久化或前端；`api/openapi.yaml` 无需改动。
