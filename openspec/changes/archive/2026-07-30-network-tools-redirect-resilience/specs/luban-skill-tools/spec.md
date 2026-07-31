## MODIFIED Requirements

### Requirement: Install documentation URL handling
When `luban_install_skill` fetches a single-file (`.md`) URL whose body is not a valid SKILL.md (no YAML frontmatter, or frontmatter missing `name`/`description`), the tool SHALL NOT treat this as an install failure. It SHALL return a structured, non-error result identifying the content as an install document and including the fetched body, so the calling agent can read it, determine the real skill source, and call `luban_install_skill` again with that source. A body that does carry valid skill frontmatter is still installed as a skill as before. When the single-file fetch fails with a non-200 status, a non-text body, or an unresolvable/excessive redirect chain, the tool SHALL return an error (not an install-doc result), and that error SHALL include the HTTP status code and, when any redirect occurred, the last redirect `Location` (the final response `Location` header when present, otherwise the last followed redirect target), so the calling agent can read the target and retry with the resolved HTTPS URL.

#### Scenario: Non-skill markdown returned as install doc
- **WHEN** `luban_install_skill` is called with `https://skillhub.cn/install/skillhub.md`
- **AND** the fetched body is prose describing how to install `gildata-finance-data`, with no valid skill frontmatter
- **THEN** the tool returns a result with `installed: false` and `kind: "install-doc"`
- **AND** the result includes the fetched `content` and a hint to read it and re-install from the real source
- **AND** nothing is written to the user skills directory

#### Scenario: Valid SKILL.md still installs
- **WHEN** `luban_install_skill` is called with a `.md` URL whose body has valid skill frontmatter
- **THEN** the tool installs it as a single skill as before (existing behavior unchanged)

#### Scenario: Non-200 or non-text response errors with redirect location
- **WHEN** the single-file fetch's final response is a non-200 status or a non-text body
- **THEN** the tool returns an error rather than an install-doc result
- **AND** the error includes the HTTP status code
- **AND** if any redirect occurred during the fetch, the error includes the last redirect `Location`

#### Scenario: Redirect loop or exceeded cap surfaces last location
- **WHEN** the single-file fetch follows a redirect chain that exceeds the standard cap (10) or forms a loop
- **THEN** the tool returns an error
- **AND** the error includes the last redirect `Location`, so the calling agent can retry with the resolved URL

## ADDED Requirements

### Requirement: luban_install_skill description guides error recovery
`luban_install_skill` 的工具描述 SHALL 告知模型：当单文件（`.md`）下载因重定向或非 200 状态而失败时，返回的错误携带 HTTP 状态码与最后一次重定向 `Location`；模型可改用解析出的 HTTPS URL 重新调用 `luban_install_skill`，必要时先调用 `webfetch` 探测最终地址与响应头以确定真实源 URL。

#### Scenario: 工具描述包含重定向/错误恢复指引
- **WHEN** `luban_install_skill` 工具被注册并渲染给模型
- **THEN** 工具描述中包含关于"失败错误带状态码与 Location、改用解析出的 HTTPS URL 重试、可先用 webfetch 探测最终地址"的指引
