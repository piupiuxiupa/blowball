## Context

`internal/tool/mcpclient/` 在三条传输路径上用 `bufio.Scanner` 读取换行/SSE 分隔的 JSON-RPC，并把单行 buffer 封顶在 `1024*1024`（1 MiB）：

- `http.go` `readSSEData` — Streamable HTTP 把整条 JSON-RPC 响应放在一条 SSE `data:` 行里。
- `sse.go` `readLoop` — SSE 长连接按行读事件。
- `stdio.go` `readLoop` — 子进程 stdout 按行读消息。

MCP Streamable HTTP 协议规定 `tools/call` 的整条 JSON-RPC 响应位于一条 `data:` 行；当返回体（大文件读取、base64 二进制/图片、超大查询结果）的单行超过 1 MiB 时，`bufio.Scanner` 返回 `bufio.ErrTooLong`（消息 `"bufio.Scanner: token too long"`），对外拼成含义不明的 `http tools/call: read sse data: bufio.Scanner: token too long`。per-user MCP 仅允许 HTTP transport，故线上命中的就是 HTTP 路径；SSE/stdio 是同模式的潜在隐患。

## Goals / Non-Goals

**Goals:**
- 三条传输路径都不再以固定单行长度截断消息，大返回体被完整读取。
- 保留一道**显式、清晰**的上限，防止失控/恶意 server 用无换行的超长流耗尽内存；超限时返回可定位的错误，而非 `token too long` 或静默截断。
- 用测试覆盖 ">1 MiB 单行" 与 "超过上限" 两种情况。

**Non-Goals:**
- 不改变 MCP 协议行为、JSON-RPC 序列化、session/header/重试逻辑。
- 不引入新的配置项（上限为包级常量；是否提升为 `tools.user_mcp.max_response_bytes` 列为开放问题）。
- 不改动 operator-global 与 per-user 的分流、认证注入、超时配置。

## Decisions

### 决策 1：用 `bufio.Reader` 替换 `bufio.Scanner`
用 `bufio.Reader.ReadString('\n')`（或 `ReadBytes('\n')`）逐行读取，它没有单 token 长度上限，仅在内存/可用数据范围内增长。这是 Go 处理不定长行的标准做法。

**备选**：把 `scanner.Buffer` 的 max 调大（如 16 MiB）。否决——仍是固定上限，超过后同样失败、同样报含义不明的错；只是把阈值推迟。

### 决策 2：上限为共享包级常量 `maxMessageBytes = 32 << 20`（32 MiB）
- 与原来的隐式 1 MiB 相比留 32 倍余量，覆盖绝大多数合法 tool 结果（文件/图片/批量数据），同时对失控流做出有界承诺。
- HTTP `readSSEData`、SSE/stdio `readLoop` 统一复用共享助手 `readCappedLine(br, maxBytes)`，按行施加累计字节数上限。实现中三条路径都用同一机制（而非为 HTTP 单独套 `io.LimitReader`）：因为承载整条消息的 `data:` 行本身就是 HTTP 路径上需要封顶的对象，对它逐行封顶等价于对整条一次性响应封顶；为便于低成本测试超限路径，`readSSEData` 拆出 `readSSEDataLimited(r, limit)`，测试用小 `limit` 断言超限行为。
- SSE/stdio `readLoop` 是长生命周期的流，单行超过 `maxMessageBytes` 即返回清晰错误并终止读循环（错误经既有的 `errCh` 传出，下次调用触发既有重连/重启路径）。
- 单一常量同时承担 HTTP 的"单消息上限"与 SSE/stdio 的"单行上限"，语义统一为"一条消息的最大字节数"。

### 决策 3：超限错误必须清晰、可归因
超限错误格式如 `mcp response too large (exceeds N bytes)`；各 transport 既有的 `http tools/call:` / `sse tools/call:` / `stdio tools/call:` 包装继续提供上下文。最终对外形如 `http tools/call: read sse data: mcp response too large (exceeds 33554432 bytes)`，可被直接定位。不得再以 `bufio.Scanner: token too long` 形式出现。

### 决策 4：常量与读取助手集中放置
新增包内共享的逐行读取助手（带单行字节上限），放在 `internal/tool/mcpclient` 内（如新增 `limits.go` 或并入既有文件），三处复用，避免重复实现与上限漂移。

## Risks / Trade-offs

- [内存上界抬高：单消息最高 32 MiB（原 1 MiB），并发 per-user turn 下可能并存多份] → 显式常量做硬上界；32 MiB 在后端可接受；与既有 `call_timeout` 同属信任边界内的防御。
- [per-user 用户可指向返回超大消息的 server] → 上限有界；与既有 per-user MCP 信任边界一致（已限 transport/凭据类型并有超时）。
- [SSE/stdio 单行上限可能误杀合法的超大流式事件] → 32 MiB 已留足余量；若实际出现可提升常量或转为配置项。
- [改动触及 operator-global 与 per-user 共用的传输层] → 行为变更是"更宽松 + 更清晰的错误"，严格向后兼容；既有测试不应回归。

## Migration Plan

- 纯代码与测试变更，无配置/持久化/协议迁移；部署 = 重新构建，回滚 = 还原提交。
- 行为差异：原本因 >1 MiB 而失败的大返回体将成功返回；真正超过 32 MiB 的返回由"含义不明的 token-too-long"变为"清晰的超限错误"。两者均向后兼容。

## Open Questions

- 是否将 `maxMessageBytes` 提升为可配置（如 `tools.user_mcp.max_response_bytes`）？当前先用常量；若运维有调参需求再提升，二者择一即可。
- operator-global 与 per-user 是否需要不同上限？当前共用同一常量；如有差异化需求再拆分。
