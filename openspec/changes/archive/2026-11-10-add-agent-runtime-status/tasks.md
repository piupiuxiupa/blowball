## 1. Characterization tests and runtime-status contract

- [x] 1.1 在 `internal/prompt` 为独立 `<current_status>`、`<tool_budget>` 和保守测量消息添加测试，固定字段顺序、RFC3339 时间、倒计时 clamp、无预算时省略 context 数值及“最新状态优先”表述。
- [x] 1.2 在 agent 连续请求测试中建立新的核心断言：上一完整请求是下一请求的逐 byte 前缀，旧状态不被修改；并行 tool results 完整到齐后才追加 tool_budget。
- [x] 1.3 在 turn 持久化测试中增加新用户原文不被状态污染、旧式内联 `<current_status>` 历史行逐 byte 重建的兼容用例。

## 2. Redis-only append-only status store

- [x] 2.1 定义独立 `runtime_status:{session_id}` 条目模型与 store 接口，包含确定性 status_id、kind、不可变内容、seq、before_client_msg_id/after_tool_call_id 锚点及同族 TTL；不改变 `msgs:{sid}` 的现有 durable 行语义。
- [x] 2.2 实现 root 状态 append、读取、完整锚点校验和按原位置合并：before-user 插到真实用户前，after-tool 插到对应最后一条 tool result 后，同锚点按 seq 排序。
- [x] 2.3 实现新缓存纪元降级：key miss/读失败/任一条目无效或锚点缺失时整份日志作废并 best-effort 清理，禁止部分重放；`DeleteSession` 同步清理状态 key。
- [x] 2.4 为 append 幂等、TTL 刷新、Redis 写失败不阻塞 turn、缓存与 durable 短暂不一致、全量校验失败及删除清理添加 Redis/service 单元测试。

## 3. Separate durable user input and create current status

- [x] 3.1 修改 `internal/turn/executor.go`，使用未拼接的 `req.Content` 构造真实用户消息、`PersistInfo.UserContent` 和标题输入，移除原 `req.Content + prompt.RenderStatus()` 行为。
- [x] 3.2 在 root system/tools 构建完成后捕获固定 turn 时间戳，生成位于真实用户消息之前的 current_status，并以 `{trace_id}:0` 为 before-user 锚点同步加入 active transcript 与 Redis 状态日志。
- [x] 3.3 实现包含 Redis 重放旧状态、新状态、system/tools、真实用户及协议脚手架的保守上下文估算；验证余量不过报、超预算为零、无有效预算时不伪造数值。
- [x] 3.4 验证附件图片和 `collectLiveMessages` 仍识别真实当前用户，标题 cadence、真实用户 ordinal 与 memory capture 不观察 synthetic 状态。

## 4. Confucius append-only integration

- [x] 4.1 在 root 主循环中保持 active transcript 只追加，工具组完成后按最后一个 tool_call_id 追加不可变 tool_budget；瞬时重试复用冻结请求且不追加状态。
- [x] 4.2 将 length continuation 的新逻辑请求接入同一 append-only 路径，以新工具组序号记录状态但保持外层 rounds_used；现有 canonical 续写脚手架和 round-cap wrap-up 不变。
- [x] 4.3 在 preflight 和 provider-informed next-request estimate 中包含 active transcript 的全部状态及即将新增状态，确认状态自身不会绕过预算。
- [x] 4.4 添加跨 root turn 集成测试：Redis 命中时原样重放状态并保持上一完整请求前缀；Redis miss/锚点失效时从 MySQL 干净历史开始新纪元。

## 5. SubAgent and protocol integration

- [x] 5.1 在通用 SubAgent 单次 Run 内 append-only 添加该实例独立的 tool_budget，不写 root Redis 状态日志；并行同名实例和不同 max_rounds 的计数保持隔离。
- [x] 5.2 为 SubAgent snapshot/lastMessages 建立明确 durable 投影，确保 `subagent_runs.messages_json` 不包含 synthetic 状态，重试不重复追加。
- [x] 5.3 更新 `terminalResponseFormat` 使用 durable/canonical 投影或显式状态类型，添加 output_schema 回归测试，确保工具结果后的 tool_budget 不关闭 structured output。
- [x] 5.4 验证 tool-call 配对、live overlay、SSE 和消息批写均通过类型/投影排除状态，而不是依赖内容字符串过滤。

## 6. Compaction and cache-epoch integration

- [x] 6.1 在 mid-turn/preflight compaction 成功路径清理 session 旧状态 key，并从 durable compacted projection 构造新的 active transcript；checkpoint 和压缩摘要不得包含 synthetic 状态。
- [x] 6.2 在压缩后追加基于新 projection 计算的 current_status；候选预算只计入压缩后新状态，不保留将被删除的旧状态开销。
- [x] 6.3 验证 incremental persister、mid-turn flush 和 terminal batch 不读取、不重复追加 Redis 状态；锚点因 crash 未持久化时下次全量校验安全作废。
- [x] 6.4 验证 compaction、Redis miss、system/tools 变化只造成明确允许的缓存断点，不影响 durable 会话正确性；通过连续 `llm_raw_log` 请求核对实际前缀。
- [x] 6.5 修复永久 `ContextCompacted` 标记导致每轮清空状态的问题：保留 stitched row attribution，稳定 checkpoint 连续 turn 正常重放，只有实际 compaction replacement 才切换缓存纪元。
- [x] 6.6 在 `RequestOverheadTokens` 中预留保守 current_status 开销，确保候选 planner 提前缩减 retained tail，最终含新状态的请求仍满足预算。
- [x] 6.7 修复压缩后序号重复：重放消息携带真实 `RuntimeStatusSeq`，后续状态按日志最大序号续接，覆盖 `[2] → [2,3] → 第三 turn 正常重放` 集成用例。

## 7. Preserve MCP server/tool activity through compaction

- [x] 7.1 在 `internal/service/tool_activity_test.go` 添加 `mcp_call` 构建测试：同一 server 的多个 tool、不同 server 的同名 tool、调用/成功/失败计数，以及普通工具和其他 `mcp_*` 工具口径不变。
- [x] 7.2 扩展 `ToolActivity` 的可选 JSON 嵌套明细和 `BuildToolActivity`，从原始 arguments 解析 `server`/`tool`；无效或缺失参数只增加父级及未归因计数，不丢失调用或伪造名称。
- [x] 7.3 更新 `MergeToolActivity`、`RenderToolActivityLedger` 与总数计算：按 `(server, tool)` 确定性合并和分组渲染，顶层总数不重复累计子项，旧记录保留未归因余量。
- [x] 7.4 添加再次压缩与 JSON 兼容测试，验证新旧台账混合后的计数守恒、旧二进制可忽略的 additive 字段、每个 server/tool 均保留且不做 Top-N 丢弃。
- [x] 7.5 在 compaction candidate/budget 测试中覆盖高基数 MCP 明细，确认优先缩减 retained tail/examples，无法拟合时走既有 uncompactable 路径，并确认压缩摘要提示词无需修改。

## 8. Documentation and full verification

- [x] 8.1 同步 `CLAUDE.md` 与 `AGENTS.md`，记录 durable canonical/active transcript、Redis append-only 状态与缓存纪元、max_rounds 倒计时、用户原文持久化、上下文估算及 MCP 嵌套 Tool Activity 规则。
- [x] 8.2 运行 `go test ./internal/agent/... ./internal/turn/... ./internal/handler/... ./internal/prompt/... ./internal/service/... ./internal/store/redis/... ./test/integration/...`，修复并复验所有相关回归。
- [x] 8.3 运行 `make test`（含 race detection）与 `make lint`，确认全量验证通过。
- [x] 8.4 运行 OpenSpec 校验并核对每条 delta requirement 均有实现与测试对应，不勾选未实际验证的任务。
