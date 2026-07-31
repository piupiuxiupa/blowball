## ADDED Requirements

### Requirement: Bounded redirect following
`webfetch` SHALL 自动跟随 HTTP 重定向，但 SHALL 将跟随次数限制在可配置的 `max_redirects` 以内（默认 10；零值或负值回退为 10）。当重定向次数达到上限时 SHALL 停止跟随并返回错误，且该错误 SHALL 同时包含"已达重定向上限"的提示与最后一次重定向目标地址（`Location`）。重定向跳数在上限以内的合法重定向链 SHALL 仍被自动跟随，行为与既有"Follow redirects"一致。此上限与 Go 标准库默认行为对齐，修正既有实现中 `CheckRedirect` 恒返回 `nil`、无上限跟随导致重定向死循环耗到超时的问题。

#### Scenario: 上限内的重定向链被自动跟随
- **WHEN** `webfetch` 的目标 URL 返回一条跳数 ≤ `max_redirects` 的重定向链并最终返回 2xx
- **THEN** 系统自动跟随整条链，返回最终 URL 的 2xx 响应（与既有行为一致）

#### Scenario: 超出重定向上限返回带最后一次 Location 的错误
- **WHEN** `webfetch` 的目标 URL 的重定向链跳数超过 `max_redirects`（默认 10）
- **THEN** 系统停止跟随并返回错误
- **AND** 错误信息包含 "stopped after N redirects"（N 为上限值）与最后一次重定向目标地址

#### Scenario: 可配置的重定向上限生效
- **WHEN** 配置 `tools.webfetch.max_redirects: 3`
- **AND** `webfetch` 的目标 URL 返回一条 5 跳的重定向链
- **THEN** 系统在第 3 次重定向后停止跟随并返回错误

#### Scenario: 零值或负值的 max_redirects 回退默认上限
- **WHEN** `max_redirects` 未配置，或配置为 0 或负值
- **THEN** 系统使用默认上限 10

### Requirement: webfetch description guides error recovery
`webfetch` 的工具描述 SHALL 告知模型：当请求最终返回非 2xx（含因达到重定向上限而未被跟随的重定向）或请求失败时，结果携带最终 HTTP 状态码与响应头（含 `Location`），模型可据此读取重定向目标、改用解析出的最终 URL 或调整后的 method/headers 重新调用 `webfetch`。

#### Scenario: 工具描述包含重定向/错误恢复指引
- **WHEN** `webfetch` 工具被注册并渲染给模型
- **THEN** 工具描述中包含关于"读取返回的状态码与 Location，并以解析后的 URL 或调整后的 method/headers 重试"的指引
