# webfetch Specification

## Purpose

TBD

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
