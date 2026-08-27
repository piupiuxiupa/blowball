# Delta Spec: workspace-api

## MODIFIED Requirements

### Requirement: Skills list

系统 SHALL 提供接口返回用户 skills 列表。当 `skill_market.url` 启用时（见 `skill-market` 能力），响应 SHALL 合并市场 allowlist 条目：市场条目的 `name`/`description` 取自市场 API、`location` 为 `"skill_market"`；名字冲突时本地条目胜出。市场拉取 SHALL 使用该 HTTP 请求自身的 `Authorization` 头（token 与 login 返回一致），不需要 turn context 管道；拉取失败时 SHALL 降级为仅本地列表（fail-closed，WARN），接口仍返回 HTTP 200。

#### Scenario: List skills

- **WHEN** 用户发送 GET /api/v1/skills
- **THEN** 系统扫描 data/{user_uuid}/skills/ 目录，返回文件列表作为可用 skills

#### Scenario: No skills

- **WHEN** 用户 skills 目录为空，且市场功能关闭或 allowlist 为空
- **THEN** 系统返回 HTTP 200，body 为空数组 []

#### Scenario: Market entries merged and labeled

- **WHEN** `skill_market.url` 已启用，市场服务对该用户的 JWT 返回含 `fund-promo-sentiment-v2` 的清单
- **THEN** 响应包含该条目，`location` 为 `"skill_market"`，`description` 为市场 API 返回值

#### Scenario: Local entry wins on name conflict

- **WHEN** 用户目录存在技能 `foo`，市场 allowlist 亦含名为 `foo` 的技能
- **THEN** 响应中 `foo` 只出现本地版本，市场条目被覆盖

#### Scenario: Market failure degrades to local-only

- **WHEN** 市场服务不可达或返回非 200
- **THEN** 接口返回仅含本地技能的列表，HTTP 200，系统记 WARN
