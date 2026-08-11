## 1. 共享上限与逐行读取助手

- [x] 1.1 在 `internal/tool/mcpclient`（新增 `limits.go`）定义包级常量 `maxMessageBytes = 32 << 20`（32 MiB），并补注释说明其为单条消息/单行的硬上界。
- [x] 1.2 实现带单行字节上限的逐行读取助手 `readCappedLine(br, maxBytes)`（基于 `bufio.Reader.ReadByte`，单行累计字节数超过上限时返回 `errMessageTooLarge` 哨兵错误），供 HTTP/SSE/stdio 三条路径复用，避免上限漂移。

## 2. HTTP 传输 `readSSEData`

- [x] 2.1 重写 `internal/tool/mcpclient/http.go` 的 `readSSEData`：改用 `bufio.NewReader` + `readCappedLine(maxMessageBytes)`，完整读取首条 `data:` 行并返回；单行超过上限时返回 `errMessageTooLarge`（实现统一用共享助手逐行封顶，等价于对承载整条消息的 `data:` 行施加 `io.LimitReader` 的效果；拆出 `readSSEDataLimited(r, limit)` 便于低成本测试超限路径）。
- [x] 2.2 移除 `readSSEData` 中的 `bufio.Scanner`，确认该路径不再产生 `token too long`。

## 3. SSE 传输 `readLoop`

- [x] 3.1 重写 `internal/tool/mcpclient/sse.go` 的 `readLoop`：改用共享助手按行读取，移除 `bufio.Scanner`，保留既有的 `stopCh` / 事件拼装 / `dispatchEvent` / 空行派发逻辑。
- [x] 3.2 单行累计字节超过 `maxMessageBytes` 时，通过既有 `errCh` 返回清晰超限错误并终止读循环（错误经 `sendRequest` 既有路径传出，下次调用触发现有重连逻辑）。

## 4. stdio 传输 `readLoop`

- [x] 4.1 重写 `internal/tool/mcpclient/stdio.go` 的 `readLoop`：改用共享助手按行读取，移除 `bufio.Scanner`，保留既有的 `stopCh` / 反序列化 / 分发逻辑。
- [x] 4.2 单行累计字节超过 `maxMessageBytes` 时，通过既有 `errCh` 返回清晰超限错误并终止读循环。

## 5. 测试

- [x] 5.1 `limits_test.go` 新增 `readCappedLine` 单元测试（读取、CRLF 剥离、超限→`errMessageTooLarge`、干净 EOF、EOF 部分行）；`http_test.go` 新增 `TestHTTPTransport_LargeSSEResponse`（2 MiB `data:` 行完整读取，复现原 bug 的回归用例）、`TestReadSSEData_Oversized`/`TestReadSSEData_LargeUnderLimit`（经 `readSSEDataLimited` 以小上限断言超限报清晰错误且不含 `token too long`）。
- [x] 5.2 `sse_test.go`：为 `testMCPSSEServer` 增加 `callText` 字段，新增 `TestSSETransport_LargeMessage`（单行 2 MiB 消息成功往返）。
- [x] 5.3 `stdio_test.go`：扩展 `testdata/stdio_server.go` 支持 `STDIO_BIG_RESULT_BYTES` 环境变量钩子，新增 `TestStdioTransport_LargeMessage`（单行 2 MiB 成功往返）。

## 6. 验证

- [x] 6.1 `make build` 通过。
- [x] 6.2 `go test ./internal/tool/mcpclient/... -race -count=1` 通过（33 个用例，含 6 个新增）。
- [x] 6.3 `make lint`（`go vet ./...`）通过。
