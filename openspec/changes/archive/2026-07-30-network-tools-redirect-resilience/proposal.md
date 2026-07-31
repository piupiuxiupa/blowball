## Why

网络访问类工具（`webfetch`、`luban_install_skill`）对 HTTP 重定向与网络错误的处理不够健壮，也不够透明，导致 agent 难以在遇到 `get https://xxxx.com: Moved` 这类错误时自行修正访问形式。具体三处问题：

1. `webfetch` 的 `CheckRedirect` 恒返回 `nil`（`internal/tool/webfetch/fetch.go:50-53`），**绕过了 Go 默认的 10 次重定向上限**。一次重定向死循环不会被识别，会一直跟随到 30s 超时才失败，既浪费资源又把"重定向循环"伪装成"超时"。
2. `luban_install_skill` 单文件下载路径在最终非 200 时只返回 `download returned status %d`（`internal/tool/luban/install.go:340-342`），**不带 `Location` / 重定向目标**。agent 看不到服务器想把它导向哪里，无法据错误自行调整 URL 重试。
3. 两个工具的描述都没有告诉 agent：遇到 3xx / 重定向 / 网络错误时该如何恢复（读返回的 status/Location、换 URL 或 method/headers 重试、或先用 `webfetch` 解析最终 URL 再回灌 luban）。

## What Changes

- **webfetch 重定向上限**：将恒返回 `nil` 的 `CheckRedirect` 改为"跟随至可配置上限（默认 10）后停止"。超限时返回明确错误（`webfetch: stopped after N redirects`）并携带最后一次看到的 `Location`，使 agent 既知道是循环、也知道最后一次被导向的地址。
- **webfetch 配置项**：`WebfetchConfig` 新增可选字段 `max_redirects`（默认 10，零值 → 10），与现有 `timeout` 风格一致；零行为变更。
- **luban 单文件下载错误透明化**：`luban_install_skill` 的 `.md` 下载在最终非 200 / 非文本 / 命中重定向上限时，错误信息中包含 HTTP 状态码与最后一次重定向 `Location`（若有）。不改变"合法重定向自动跟随、合法 SKILL.md 正常安装"的现有行为。
- **工具描述补充错误恢复指引**：在 `webfetch` 与 `luban_install_skill` 的工具描述中明确告知 agent 遇到 3xx / 非 200 / 重定向错误时如何利用返回的状态码与 `Location` 自行恢复（调整 URL/method/headers 重试，或先用 `webfetch` 解析最终地址再重试 luban）。

非破坏性变更：合法重定向仍被自动跟随，现有成功路径行为不变；只是失败路径更明确、可恢复。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `webfetch`：新增"有限重定向上限"要求（默认 10、可配置），并要求重定向超限/未跟随时返回的错误与结果携带足够信息（状态码 + 最后 Location）供 agent 自行恢复；工具描述补充错误恢复指引。
- `luban-skill-tools`：`luban_install_skill` 单文件下载的非 200 / 非文本 / 重定向上限错误 SHALL 携带状态码与最后一次重定向 `Location`（若有）；工具描述补充错误恢复指引。

## Impact

- 代码：
  - `internal/tool/webfetch/fetch.go`（重写 `CheckRedirect` 为带上限的实现，超限错误携带 Location）。
  - `internal/tool/webfetch/register.go`（把 `MaxRedirects` 透传给 `Fetch`；更新工具描述）。
  - `internal/config/config.go`（`WebfetchConfig` 新增 `MaxRedirects`，默认值应用）。
  - `internal/tool/luban/install.go`（`installer` 下载 client 增加记录重定向链的 `CheckRedirect`；`download` 非 200/非文本错误携带状态码与 Location）。
  - `internal/tool/luban/register.go`（更新 `luban_install_skill` 工具描述）。
- 配置：`config.example.yaml` 增补 `tools.webfetch.max_redirects` 示例（注释、默认 10）。
- 测试：`internal/tool/webfetch/fetch_test.go`（重定向上限、超限错误携带 Location）、`internal/tool/luban/luban_test.go`（单文件下载非 200 错误携带 Location）。
- 不涉及 API 契约（`api/openapi.yaml`）变化，不涉及数据库迁移，不影响 SSE/持久化/角色划分。
