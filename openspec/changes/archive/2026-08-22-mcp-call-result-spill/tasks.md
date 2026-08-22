# mcp-call-result-spill — Tasks

## 1. 共享 token 估算器提取

- [x] 1.1 新建 `internal/tokens` 包，将 `internal/handler/tokens.go` 的 `estimateTokens`/`isCJK` 迁入并导出（`Estimate`），注释保留 heuristic 定位与精度说明
- [x] 1.2 `internal/handler/tokens.go` 改为对共享包的薄引用（或删除并全量替换调用点 `message_stream.go`），既有估算行为 byte-for-byte 不变
- [x] 1.3 为 `internal/tokens` 补单测（CJK/ASCII/混合/空串边界），跑 `go test ./internal/handler/...` 确认输入限行为不回归

## 2. 配置项与接线

- [x] 2.1 `internal/config/config.go`：`UserMCPConfig` 增加 `MaxInlineResultTokens *int` + 取值方法（nil → 20000，显式 0 → 0），`validate()` 拒绝负值
- [x] 2.2 `config.example.yaml` 增加 `tools.user_mcp.max_inline_result_tokens` 注释示例（默认/关闭/负数拒绝语义）
- [x] 2.3 沿 `tools.user_mcp` 既有接线把值传入 per-turn `ManagerOptions`/`Manager`/`Tools`（`cmd/blowball/serve.go` → orchestrator factory），配置单测覆盖 nil/0/负/正值

## 3. spill 落盘实现（internal/tool/mcp）

- [x] 3.1 导出或复用 xizhi 的路径校验 helper，`output_path` 做同规 confinement（绝对路径/`..`/symlink/`.blowball` 前缀拒绝、workspace 内清理），单测覆盖每种拒绝形态
- [x] 3.2 实现 spill 写入器：`tmp/mcp-outputs/{server}/` 命名（`{tool}-{trace_id}-{seq}`，Manager 上原子计数器，trace_id 取自 ctx、缺失退化时间戳）、单 text item 写原始文本（.txt）/否则 JSON 数组（.json）、`MkdirAll` 父目录、自动路径 create 不覆盖、`output_path` create-or-overwrite
- [x] 3.3 实现 preview 截取（头 1KB、rune 安全）与信封构造（`spilled/path/tokens_est/size/items/preview/hint`）；单测覆盖并行两 spill 文件名唯一、跨 turn trace_id 区分

## 4. mcp_call 集成

- [x] 4.1 `registerCall`：schema 增加 `output_path` property（含用途说明），description 增加 steering 句（自动落盘 + 主动参数引导）
- [x] 4.2 `callTool` 返回路径接入：对 marshal 后 `callResult` 做 `tokens.Estimate` → 超阈值或指定 `output_path` 时落盘并返回信封；阈值内且未指定 → 返回形状与现状一致
- [x] 4.3 `output_path` 校验失败在远端调用前拒绝（明确错误，无远端副作用）
- [x] 4.4 落盘写失败降级：`{truncated:true, size, preview, note}`，绝不返回全量；远端成功保持 status 0
- [x] 4.5 显式 `0` 关闭时任意大小结果 inline（byte-for-byte 现状），`isError` 错误路径不参与判定

## 5. 测试与验证

- [x] 5.1 `internal/tool/mcp` 单测：超阈值 spill（信封字段、文件内容格式）、阈值边界（恰好等于）、`output_path` 无条件落盘与覆盖、写失败降级、关闭开关
- [x] 5.2 集成验证：`make test` 全绿 + `make lint`
- [x] 5.3 文档：`CLAUDE.md` 的 per-user MCP 段落补 spill 行为一句话（含 `tmp/mcp-outputs/` 位置与配置项）
