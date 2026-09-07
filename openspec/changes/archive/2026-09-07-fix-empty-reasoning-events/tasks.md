## 1. 流式解析修复

- [x] 1.1 在 `internal/agent/openai_client.go` 的 `StreamChat` reasoning 分支中，为 unmarshal 结果增加 `rc != ""` 守卫：空字符串不 `WriteString`、不调用 `onReasoning`（对齐 content 分支的判空）
- [x] 1.2 新增回归测试：SSE 流包含"非空 reasoning chunk + 多个 `content` 与 `reasoning_content:\"\"` 混合 chunk"，断言 `onReasoning` 仅收到非空 delta、`onToken` 收到全部 content、`resp.ReasoningContent` 无空串污染

## 2. 验证与交付

- [x] 2.1 运行 `go test ./internal/agent/ ./internal/handler/`，确认既有 reasoning/token/合并相关测试不受影响
- [x] 2.2 运行 `make lint`
- [x] 2.3 在 change 交付说明中附上独立存量清理 SQL（不加入 `migrations/`），包含复核 SELECT 与事务化 DELETE
