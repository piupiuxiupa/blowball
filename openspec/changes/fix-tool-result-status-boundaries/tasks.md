## 1. 锁定生产基线并建立组合回归

- [x] 1.1 确认实施工作树由 `9b18297e82732e095f479978c184448cd4b3ee58` 创建，差异仅包含本 change；记录生产基线的相关包测试结果，不带入主工作区的后续提交或调度器改动。
- [x] 1.2 在 turn/handler 重建测试中构造脱敏最小 fixture：上一 turn 的 tool-only 终稿及已完成 A，下一 turn 的 recall-B placeholder/tool-call/user 行，以及锚定 A 的 budget；断言两个独立工具单元及合法 active 顺序，先运行并确认生产代码下失败。
- [x] 1.3 为组合 fixture 保留原调用 ID、arguments/result 和用户正文，增加本地工具配对及请求前缀断言；不提交生产日志、配置凭据或自动发送线上请求的测试。

## 2. 修复 canonical 工具单元边界

- [x] 2.1 在 `internal/turn/message_reconstruct.go` 的重建状态中跟踪当前 assistant/tool 单元已知的来源 TraceID；不同已知 turn 的 placeholder 到达时先关闭前一单元，无 token/reasoning 时同样生效，关闭后清理单元身份。
- [x] 2.2 验证上一 turn 的 assistant 终稿/content/reasoning 不被附着到下一 turn 的 synthetic recall；保留原字节、call/result 顺序及当前真实 user 和 assistant final-row 的边界。
- [x] 2.3 补充同轮多个 tool-only placeholder、narrated 并行工具、同 TraceID 连续 tool-only round、空 completed result 和缺失 TraceID 遗留行测试，确保不按 `afterResult`、工具名、ID 前缀或执行完成顺序拆组。
- [x] 2.4 验证拆分后的 assistant/tool row attribution 只覆盖自身来源行，运行已有混合遗留/终稿前缀及压缩完整工具单元测试，确认 canonical 重建回归由红转绿。

## 3. 校验 runtime status 的完整组锚点

- [x] 3.1 在 `internal/turn/runtime_status_test.go` 先添加失败用例：组中间 after-tool 锚点、未完成工具组、非真实 user 或非唯一 before-user 锚点、kind/锚点类型不匹配，以及锚点造成的 Seq 回放逆序。
- [x] 3.2 在 `replayRuntimeStatus` 中根据 assistant 声明和连续匹配结果识别完整工具组，只接受唯一的组尾 after-tool 锚点与真实 user before-user 锚点；校验候选状态顺序和恰好一次插入，任一失败拒绝整份日志，不重排或重锚定旧状态。
- [x] 3.3 覆盖有效并行组尾、原状态字节/Seq 保留与有间隔 Seq 从最大值续接；证明合法缓存仍保留完整 live 前缀，canonical 输入未被修改。
- [x] 3.4 验证 `replaySessionRuntimeStatus` 沿用 WARN、best-effort 清理和新 epoch 降级；缓存读取/清理失败均不使用失效状态、不阻断 turn，不改变真实用户 ordinal 或 durable 记录。

## 4. 兼容性验证与交付

- [x] 4.1 用可控阻塞的假工具验证 root/SubAgent 真正并行调用：一个工具先完成、另一个仍阻塞时不发起后续模型请求；全部完成后按调用序追加全部结果，并只在完整组后追加一次 budget，保持 `max_rounds` 计数不变。
- [x] 4.2 通过实际请求序列化路径验证“上一工具尾部 → 下一 recall pair → status replay → 新 user”的恢复请求；验证调用/结果配对、两个独立单元、current status 位置、合法日志的逐 byte 前缀，以及状态仍不进入 MySQL、`msgs:{session_id}`、SSE 或标题/记忆输入。
- [x] 4.3 运行相关包筛选测试与 `go test -race ./internal/agent ./internal/turn ./internal/handler`，再运行 `make test`、`make lint` 和 `openspec validate fix-tool-result-status-boundaries --strict --no-interactive`，记录完整结果；Go 修改遵循生产基线的版本指导与 gofmt。
- [x] 4.4 将 proposal/design/specs/tasks 与实际行为同步，确认无 HTTP/OpenAPI、SQL、Redis schema、配置或依赖变更；通过测试后创建仅包含本修复源码、测试和文档的 scoped Conventional Commit，不提交日志、凭据或构建产物，不自动归档 change。
