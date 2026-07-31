# Implementation Tasks

## 1. webfetch 配置与重定向上限

- [x] 1.1 `internal/config/config.go`：`WebfetchConfig` 新增字段 `MaxRedirects int \`yaml:"max_redirects"\``。（webfetch 无 config 侧默认块——与 `timeout` 一致，默认值在 `Fetch` 内零值兜底为 10。）
- [x] 1.2 `internal/tool/webfetch/fetch.go`：`Fetch` 签名追加尾参 `maxRedirects int`；函数内 `if maxRedirects <= 0 { maxRedirects = defaultMaxRedirects }`（新增常量 `defaultMaxRedirects = 10`）；用闭包替换恒 `nil` 的 `CheckRedirect`：每次记录 `lastLocation = req.URL.String()`，并在 `len(via) >= maxRedirects` 时 `return fmt.Errorf("stopped after %d redirects", maxRedirects)`，否则返回 `nil`。
- [x] 1.3 `internal/tool/webfetch/fetch.go`：`client.Do` 返回错误时，若 `lastLocation != ""`，在错误信息尾部追加 `; last redirect location: <lastLocation>`（保持 `webfetch: request failed: ...` 前缀与超时分支不变）。
- [x] 1.4 `internal/tool/webfetch/register.go`：`Fetch` 调用点透传 `cfg.MaxRedirects`。

## 2. webfetch 工具描述

- [x] 2.1 `internal/tool/webfetch/register.go`：在 `Description` 末尾追加错误恢复指引文案（非 2xx/失败时结果带最终状态码与 headers 含 `Location`，可据此读 Location、改用解析出的 URL 或调整 method/headers 重试）。

## 3. luban 单文件下载错误透明化

- [x] 3.1 `internal/tool/luban/install.go` `download`：在函数内声明 `var lastLocation string`，浅拷贝 `client := *ins.httpClient`，为其设置每调用独立的 `CheckRedirect`（记录 `lastLocation`，`len(via) >= defaultDownloadRedirects`（=10）时返回 `stopped after 10 redirects`，否则 `nil`）。
- [x] 3.2 `internal/tool/luban/install.go` `download`：`client.Do` 错误分支在信息中带上 `lastLocation`（非空时追加 `; last redirect location: <lastLocation>`）。
- [x] 3.3 `internal/tool/luban/install.go` `download`：非 200 分支组装错误信息时携带状态码与 Location——优先取 `resp.Header.Get("Location")`，否则用 `lastLocation`，格式如 `download returned status %d; redirect location: %s`。

## 4. luban 工具描述

- [x] 4.1 `internal/tool/luban/register.go` `registerInstallSkill`：在 `Description` 末尾追加错误恢复指引文案（单文件下载因重定向/非 200 失败时错误带状态码与最后一次 Location；可改用解析出的 HTTPS URL 重试，必要时先用 `webfetch` 探测最终地址）。

## 5. 配置示例

- [x] 5.1 `config.example.yaml`：在 `tools.webfetch` 段补注释化示例 `# max_redirects: 10`，并说明零值/负值回退默认 10。

## 6. 测试

- [x] 6.1 `internal/tool/webfetch/fetch_test.go`：更新既有 6 处 `Fetch` 调用至新签名（尾参传 0）；新增 `TestFetch_RedirectWithinCap`（3 跳链 + maxRedirects=5 → 成功）、`TestFetch_RedirectCapExceeded`（自循环 + maxRedirects=3 → 错误含 "stopped after 3 redirects" + 最后 Location）、`TestFetch_DefaultMaxRedirects`（自循环 + maxRedirects=0 → "stopped after 10 redirects"）。
- [x] 6.2 `internal/tool/luban/luban_test.go`：新增 `TestInstallSkill_SingleFile_Non200SurfacesLocation`（302→404，错误含 "status 404" + "redirect location" + "/gone"）与 `TestInstallSkill_SingleFile_RedirectLoopSurfacesLocation`（自循环 → "stopped after 10 redirects" + "last redirect location"）。
- [x] 6.3 既有 luban/webfetch 用例回归通过（合法重定向与合法 SKILL.md 安装行为不变）。

## 7. 验证

- [x] 7.1 `go build ./...` 通过。
- [x] 7.2 `go test -race ./...`（含 `./internal/tool/webfetch/... ./internal/tool/luban/...`）通过。
- [x] 7.3 `make lint`（`go vet ./...`）通过；gofmt 已对所有改动文件 `-w` 归整。
