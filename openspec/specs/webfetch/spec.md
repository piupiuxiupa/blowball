# webfetch Specification

## Purpose

Define bounded external web retrieval for agents, including redirect safety, HTML extraction, context-size limits, and optional threshold-gated small-model digestion for large extracted documents.

## Requirements

### Requirement: Webfetch tool
系统 SHALL 提供抓取外部网页并返回文本响应的工具，所有 Agent 均可使用。

#### Scenario: Fetch HTML page
- **WHEN** Agent 调用 `webfetch`，url 为 `"https://example.com"`
- **THEN** 系统返回最终 URL、HTTP 状态码、响应头和文本响应体

#### Scenario: Follow redirects
- **WHEN** Agent 调用 `webfetch`，目标 URL 返回 302 重定向
- **THEN** 系统自动跟随重定向，并返回最终 URL 的响应

#### Scenario: Custom HTTP method and headers
- **WHEN** Agent 调用 `webfetch`，method 为 `"POST"`，headers 包含 `"Content-Type": "application/json"`
- **THEN** 系统使用指定方法和请求头发起请求

#### Scenario: Request timeout
- **WHEN** Agent 调用 `webfetch`，请求在 30 秒内未完成
- **THEN** 系统取消请求并返回超时错误

#### Scenario: Invalid URL
- **WHEN** Agent 调用 `webfetch`，url 格式不合法
- **THEN** 系统返回错误，提示 URL 无效

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

### Requirement: Bounded webfetch response and model content
`webfetch` SHALL bound response bytes retained from the network with `tools.webfetch.max_download_bytes` (default 5 MiB) and SHALL bound extracted content returned to the model with `tools.webfetch.max_output_bytes` (default 100 KiB). Zero or negative values for either field SHALL fall back to the corresponding default. A response that exceeds the download cap SHALL still return its available status, headers, and truncated content rather than failing solely because of size.

#### Scenario: Response exceeds the download cap
- **WHEN** `webfetch` receives a response body larger than `max_download_bytes`
- **THEN** the tool retains at most `max_download_bytes` response bytes
- **AND** the result reports `download_truncated: true` and `truncated: true`
- **AND** the returned body ends with a visible truncation notice

#### Scenario: Download cap defaults
- **WHEN** `max_download_bytes` is omitted, zero, or negative
- **THEN** the tool uses a 5 MiB download cap

#### Scenario: Extracted content exceeds the output cap
- **WHEN** content extracted from a response is larger than `max_output_bytes`
- **THEN** the returned body is cut to at most `max_output_bytes`
- **AND** the result reports `content_bytes` as the pre-cap extracted size and `truncated: true`
- **AND** the returned body ends with a visible truncation notice

#### Scenario: Output cap defaults
- **WHEN** `max_output_bytes` is omitted, zero, or negative
- **THEN** the tool uses a 100 KiB output cap

### Requirement: HTML responses are extracted as Markdown-oriented text
For HTML or XHTML responses, `webfetch` SHALL parse the response with charset detection, omit script, style, template, and non-content head metadata, and return a Markdown-oriented representation containing readable text and common semantic elements such as headings, links, images, lists, code, and tables. The result SHALL report `converted_from_html: true`. Non-HTML response bodies SHALL be returned as text without HTML extraction.

#### Scenario: HTML scripts and styles are omitted
- **WHEN** an HTML response contains readable text plus `<script>` and `<style>` elements
- **THEN** the returned body contains the readable text
- **AND** the returned body does not contain the script or style contents
- **AND** the result reports `converted_from_html: true`

#### Scenario: HTML semantics are retained
- **WHEN** an HTML response contains a heading and an anchor with an absolute or page-relative `href`
- **THEN** the returned Markdown-oriented body represents the heading level and links to the resolved URL

#### Scenario: Non-HTML content is not converted
- **WHEN** a response has a non-HTML text content type
- **THEN** the tool returns the text body without applying HTML extraction
- **AND** the result reports `converted_from_html: false`

### Requirement: Threshold-gated small-model content digestion
When `tools.webfetch.digest.enabled` is true, `webfetch` SHALL decide whether to invoke the configured digest model from extracted `content_bytes`, not from raw response size. Content at or below `threshold_bytes` SHALL be returned directly with no digest model call. Content above `threshold_bytes` and at or below `single_shot_max_bytes` SHALL use one digest model call. Larger content SHALL be split into bounded chunks and analyzed concurrently up to `concurrency` before being reduced. The tool SHALL accept an optional `objective` argument that tells the digest model what information to retain.

#### Scenario: Small content does not invoke the digest model
- **WHEN** digesting is enabled and extracted `content_bytes` is at or below `threshold_bytes`
- **THEN** the result has `processing_mode: "direct"` and no digest model call is made

#### Scenario: Medium content uses one model call
- **WHEN** extracted `content_bytes` is above `threshold_bytes` and at or below `single_shot_max_bytes`
- **THEN** the digest model is called once with the full extracted content and objective
- **AND** the result reports a single-shot digest

#### Scenario: Large content uses bounded concurrent map-reduce
- **WHEN** extracted `content_bytes` is above `single_shot_max_bytes`
- **THEN** the content is split into chunk files of at most `chunk_bytes`
- **AND** at most `max_chunks` chunks are accepted
- **AND** at most `concurrency` digest model calls run simultaneously
- **AND** successful chunk analyses are reduced in original order
- **AND** the result reports chunk counts, analyzed coverage, and digest status

#### Scenario: Digest cost is bounded
- **WHEN** extracted content would require more than `max_chunks` chunks
- **THEN** the tool does not expand the worker fan-out
- **AND** it falls back to bounded direct content with a skipped digest result

#### Scenario: Digest failure preserves the fetch result
- **WHEN** a digest model call fails or returns no usable content
- **THEN** `webfetch` still returns the fetched status, headers, and bounded direct content
- **AND** the digest metadata reports the failure
