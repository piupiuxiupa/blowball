## Context

`webfetch` 与 `luban_install_skill` 是 agent 进程内运行的网络工具（不在 bwrap `--unshare-net` 沙箱里，因此走宿主网络）。二者当前的重定向/错误处理存在三处薄弱点（详见 `proposal.md`）：

- `webfetch` 的 `http.Client.CheckRedirect` 恒返回 `nil`（`internal/tool/webfetch/fetch.go:50-53`），绕过 Go 默认 10 次上限 → 重定向死循环会跟随到 30s 超时。
- `luban_install_skill` 单文件下载在非 200 时只返回 `download returned status %d`（`internal/tool/luban/install.go:340-342`），不带 `Location`，agent 无法据错误自行修正 URL。
- 两工具描述无错误恢复指引。

约束：不破坏现有成功路径；不引入新依赖；零行为变更默认值；不改动 API 契约/DB/SSE/角色划分。HTTPS 支持本身已完备（webfetch 走默认 transport，luban 强制 `https`），本次只改"重定向上限"与"错误透明度"。

## Goals / Non-Goals

**Goals:**

- webfetch 恢复有限重定向上限（默认 10、可配置），超限返回明确错误并携带最后一次 `Location`。
- luban 单文件下载的非 200 / 非文本 / 重定向上限错误携带状态码与最后一次重定向 `Location`。
- 两工具描述补充"遇到 3xx/非 200/网络错误如何自行恢复"的指引。

**Non-Goals:**

- 不改动 luban 的 git-clone 路径（`defaultGitRunner` 直呼 `git`，重定向由 git 自身处理，超出本次范围）。
- 不允许 agent 关闭 TLS 校验或注入自定义 CA（自签名/过期证书仍硬失败，这是既有安全姿态，不在本次扩大）。
- 不为 luban 单文件下载新增可配置重定向上限（沿用标准 10 次）；只有 webfetch 暴露 `max_redirects`。
- 不改 webfetch/luban 的执行环境（仍在 agent 进程内、走宿主网络）。

## Decisions

### D1：webfetch 重定向上限用闭包捕获 lastLocation，超限即报错

在 `Fetch` 内构造 client 时，用自定义 `CheckRedirect` 替换恒 `nil`：

```go
var lastLocation string
client := &http.Client{
    Timeout: timeout,
    CheckRedirect: func(req *http.Request, via []*http.Request) error {
        lastLocation = req.URL.String()
        if len(via) >= maxRedirects {
            return fmt.Errorf("stopped after %d redirects", maxRedirects)
        }
        return nil
    },
}
```

- `len(via) >= maxRedirects` 的判定与标准库 `defaultCheckRedirect` 完全一致（在发出第 `N+1` 个请求前停止），因此默认 `maxRedirects=10` 时语义与 Go 默认 client 逐字相同 —— 既"改回保留 10 次上限"，又不改变任何合法短链的行为。
- 无论 `client.Do` 因何失败，只要 `lastLocation != ""`，就在错误尾部追加 `; last redirect location: <url>`。这覆盖"超限停止""循环""TLS 中断在重定向途中"等所有失败，无需 `errors.As` 解包 `*url.Error`（更简单、信息更全）。
- **备选 A（超限返回最后一次响应而非报错）**：用 `http.ErrUseLastResponse` 让客户端把 3xx 响应返回，agent 直接看到 `status_code`+`Location`。更"自描述"，但 3xx 在结果里形似"成功只是状态码非 2xx"，与"超限=异常"的语义混淆，且改变了"重定向被自动处理"的既有契约。**否决**。
- **备选 B（硬编码 10，不加配置项）**：最贴合"改回 10 次上限"。但仓库一贯偏好可配置旋钮（`timeout`/`max_output_bytes` 等），且运行在严格代理/网关后的运维可能需要收紧或放宽。**采纳可配置**，零值 → 10，默认行为不变。

### D2：`WebfetchConfig` 新增 `max_redirects`，默认值在 `Fetch` 内兜底

- `WebfetchConfig` 加 `MaxRedirects int `yaml:"max_redirects"``。
- 默认值应用方式与现有 `Timeout` 对称：`Fetch` 内 `if maxRedirects <= 0 { maxRedirects = 10 }`（`fetch.go` 已有 `if timeout <= 0 { timeout = defaultTimeout }`）。同时建议在 config 的 webfetch 默认块也填 `10`，与 timeout 默认块风格一致。
- `register.go` 把 `cfg.MaxRedirects` 透传给 `Fetch`（新增一个参数，或封装成 `FetchOptions`）。当前 `Fetch(rawURL, method, headers, timeout)` 签名已较长；**采用追加尾参 `maxRedirects int`**，避免引入新类型，改动最小、调用点单一（仅 `register.go:58`）。

### D3：luban 下载以"浅拷贝 client + 每次闭包"捕获重定向链，不改共享 client

`download` 内：

```go
var lastLocation string
client := *ins.httpClient                 // 浅拷贝：保留 Transport/Timeout/Jar
client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
    lastLocation = req.URL.String()
    if len(via) >= 10 {
        return fmt.Errorf("stopped after 10 redirects")
    }
    return nil
}
```

- **浅拷贝 `*ins.httpClient`** 是关键：既复用测试通过 `WithHTTPClient` 注入的 transport（测试可控），又能为每次调用挂一个独立的、非共享的 `CheckRedirect` 闭包，避免并发竞争（多 agent turn 并发调用 `luban_install_skill` 时共享 client 的 `CheckRedirect` 写入是数据竞争）。
- 非 200 错误信息组装：优先取最终响应的 `Location` 头（若有，例如未被自动跟随的 3xx），否则用闭包记录的 `lastLocation`，并始终带状态码：`download returned status %d, Location: %s` 或 `... status %d; last redirect location: %s`。
- **不**为 luban 暴露 `max_redirects` 配置（沿用标准 10）。

### D4：工具描述补错误恢复指引（仅文案）

- `webfetch` 描述追加一句：超限或非 2xx 时结果仍带最终状态码与 headers（含 `Location`）；agent 可读 `Location`/状态后用解析出的 URL 或调整后的 method/headers 重试。
- `luban_install_skill` 描述追加一句：单文件下载若因重定向/非 200 失败，错误带状态码与最后一次 `Location`；可改用解析出的 HTTPS URL 重试（必要时先用 `webfetch` 探测最终地址/headers）。
- 纯文案，无运行时分支变化。

## Risks / Trade-offs

- **[合法长重定向链（>10）现在会报错]** → 之前会跟到超时。这是"恢复标准上限"的预期行为；运维可通过 `max_redirects` 调大。属可接受的行为收紧，且失败信息更明确。
- **[浅拷贝 `http.Client` 拷贝了 `Jar`/`Transport` 指针]** → 多次调用共享同一 transport/jar 实例。这与原先直接用 `ins.httpClient` 的共享语义完全一致，不引入新的共享面；仅 `CheckRedirect` 变为每调用独立。无新增风险。
- **[`lastLocation` 在闭包中捕获，单次 `download`/`Fetch` 调用作用域]** → 无跨调用泄漏；闭包随每次调用新建。安全。
- **[agent 能否真的"自行调整"取决于模型]** → 本次只保证信息充足（status+Location）与描述有引导；最终是否调整仍依赖 LLM 判断力。这是设计边界，非缺陷。

## Migration Plan

- 纯代码/配置/文案变更，无 DB 迁移、无 API 契约变化、无协议变更。
- 部署：重新构建 `bin/blowball` 重启即可。默认 `max_redirects=10`，未设置该字段的旧 `config.yaml` 行为不变。
- 回滚：还原 `fetch.go`/`register.go`/`install.go`/`config.go`/描述文案即可；无副作用需清理。

## Open Questions

- 无。三处改动均独立、范围明确，默认值与现有行为一致。
