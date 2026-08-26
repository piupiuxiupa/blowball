## 1. Layer 1 — 子 agent Run 错误返回携带部分输出

- [x] 1.1 `internal/agent/chongzhi.go`：`Run` 的 llm_error 返回路径（`chongzhi: stream chat: %w`）与 ctx 取消路径改为返回 `assistantText`（失败轮已流出的 token），替代恒空的 `finalContent`；补注释说明「错误返回值携带部分输出，供 dispatchSubAgent 放弃点拼接」
- [x] 1.2 `internal/agent/liang.go`：对称修改（同样的 llm_error 与 ctx 取消路径）
- [x] 1.3 确认 `length_exhausted` 路径已返回 `result.Content`（跨 continuation 尝试累积）——仅验证，不改；`round_cap_exhausted` 路径保持返回 `finalContent`（恒空，不抢救 wrap-up 内部输出）

## 2. Layer 2 — dispatchSubAgent 放弃点拼接

- [x] 2.1 `internal/agent/confucius.go`：新增 `joinFailure(err error, partial string) string` 辅助函数 —— `strings.TrimSpace(partial) == ""` 时返回 `err.Error()`（字节级不变），否则返回 `err.Error() + "\n\n--- partial output before failure ---\n\n" + partial`
- [x] 2.2 `dispatchSubAgent` 的 4 个失败返回点改用 `joinFailure(err, content)`：`!shouldRetry` 早退、backoff 中 `ctx.Done()`、重试循环耗尽/非瞬断 break 后的最终返回（`err == nil` 成功返回与构造失败路径不变）
- [x] 2.3 核对 `retryErrorEvent`（重试提示事件）保持只带 err、不带部分输出（D4）

## 3. 测试

- [x] 3.1 单测：`joinFailure` —— 空白 partial 返回纯错误文本（字节级）、非空 partial 拼接出固定分隔标记、partial 含换行的拼接形态
- [x] 3.2 单测（chongzhi/liang，fake LLM 流式中途失败）：llm_error 时 `Run` 返回值携带已流出的 `assistantText`
- [x] 3.3 单测（confucius dispatch，fake 子 agent 返回 err + content）：非瞬断不重试路径的 tool result content 为合并文本、`isError: true`；fake 返回 err + 空 content 时 content 字节级等于错误文本（零回归）
- [x] 3.4 单测：重试耗尽路径携带的是**最后一次**尝试的部分输出；`length_exhausted`（fake 返回 LengthHit + 累积 content）走拼接后的全量部分输出
- [x] 3.5 `make test` 全绿（含 `go test ./internal/agent/...` 与 `./test/integration/...`）；`make lint` 通过

## 4. 文档

- [x] 4.1 更新 `CLAUDE.md` Agent orchestration 段落中 sub-agent 结果回传的描述：失败时返回「错误 + 已积累部分输出」，空白时保持纯错误文本
