# skill-market Specification

## Purpose

定义远端授权的第三技能来源（skill market）：`skill_market:` 配置块、以调用者 login JWT 拉取的 per-user allowlist、TTL 缓存、fail-closed 降级语义、`{data-dir}/skills-market` 磁盘布局与路径逃逸防护、`user > global > market` 的名字解析优先级、JWT 的 turn context 管道，以及磁盘同步滞后容忍。授权与内容分发分离：市场服务决定每个用户能看到哪些技能，技能实体由 operator 同步落到每台主机的共享目录，blowball 严格按远端返回的清单放行读取——授权对工具层与文件系统层（bash 沙箱）同样成立，任何失败路径均不得回退为无授权的磁盘目录扫描。

## Requirements

### Requirement: Skill market configuration block

系统 SHALL 支持顶层 `skill_market:` 配置块，字段：`url`（字符串）、`timeout`（duration，默认 `5s`）、`cache_ttl`（duration，默认 `60s`）。块省略或 `url` 为空 SHALL 等同于功能关闭，系统行为与该能力引入前逐字节一致；`url` 非空即启用，不设独立 `enabled` 开关。所有字符串值 SHALL 走既有的 `${VAR}` / `${VAR:default}` 环境展开。启用状态下 `url` MUST 为绝对 `http(s)` URL；`timeout` / `cache_ttl` 为负值时配置加载 SHALL 失败。

#### Scenario: 块省略时功能关闭

- **WHEN** 配置中不含 `skill_market` 块，或块存在但 `url` 为空
- **THEN** 不构造市场客户端、不发出任何出站请求，luban 工具、bash 沙箱、`GET /api/v1/skills` 的行为与该能力引入前逐字节一致

#### Scenario: 环境变量展开

- **WHEN** 配置为 `skill_market.url: "${MARKET_URL}"` 且环境变量 `MARKET_URL` 已设置
- **THEN** 加载后的 `url` 为该环境变量的值

#### Scenario: 非法 URL 被拒绝

- **WHEN** `skill_market.url` 非空但不是绝对 `http(s)` URL（如 `ftp://x` 或相对路径）
- **THEN** 配置加载失败，进程拒绝启动

#### Scenario: 负 duration 被拒绝

- **WHEN** `skill_market.timeout` 或 `skill_market.cache_ttl` 配置为负值
- **THEN** 配置加载失败，进程拒绝启动

### Requirement: Market allowlist fetch with the caller's login JWT

启用时，系统 SHALL 以发起会话用户 login 返回的 JWT 为凭证拉取该用户的市场技能清单：`GET {skill_market.url}`，请求头 `Authorization: Bearer <JWT>`，不携带其他参数；用户身份由市场服务从 token 解出，blowball 不传用户名。响应 SHALL 按 `{"skills":[{slug,name,description,role,path}]}` 解析：`name` 作为技能名主键、`description` 用于列表展示、`path` 用于磁盘路径拼接；`role` 字段 SHALL 被忽略（不过滤、不分支）。

#### Scenario: 成功拉取并解析清单

- **WHEN** 市场服务对该 JWT 返回 200 与 `{"skills":[{"slug":"s","name":"fund-promo-sentiment-v2","description":"…","role":"installed","path":"/019fef90-…/fund-promo-sentiment-v2"}]}`
- **THEN** allowlist 含一条技能：name=`fund-promo-sentiment-v2`，path=`/019fef90-…/fund-promo-sentiment-v2`，`role` 不影响任何行为

#### Scenario: 市场服务从 JWT 解出用户身份

- **WHEN** 用户 A 与用户 B 分别发起会话并触发清单拉取
- **THEN** 两次请求仅 `Authorization` 头中的 JWT 不同，各自得到市场服务为该用户返回的清单

#### Scenario: 鉴权失败按 fail-closed 处理

- **WHEN** 市场服务返回 401/403
- **THEN** 记 WARN 并按空 allowlist 处理（见 fail-closed 需求），不重试、不中断调用方工具

### Requirement: Per-user allowlist TTL cache

系统 SHALL 以 userID 为 key 缓存解析后的 allowlist，TTL 为 `skill_market.cache_ttl`（默认 60s）；TTL 窗口内同一用户的多次工具调用 SHALL NOT 产生额外出站请求。缓存 SHALL 只保存 API 返回值（name/description/path），不掺入磁盘存在性状态——磁盘检查在使用点进行。

#### Scenario: TTL 窗口内单次出站

- **WHEN** 同一用户在一分钟内先后调用 `luban_list_skills` 与 `luban_read_skill`（市场技能）
- **THEN** 只向市场服务发出一次清单请求，第二次调用使用缓存

#### Scenario: 缓存过期后重拉

- **WHEN** 距上次拉取已超过 `cache_ttl` 且再次需要 allowlist
- **THEN** 系统重新请求市场服务并刷新缓存

### Requirement: Fail-closed degradation

市场 URL 不可达、超时（`timeout` 预算内未完成）、返回非 200、响应解析失败或鉴权失败时，系统 SHALL 记一条 WARN 并按空 allowlist 继续：市场技能不出现在任何列表中、市场技能读取返回 "not found" 类错误、bash 沙箱不挂载任何市场目录。系统 SHALL NOT 在任何失败路径下回退为扫描 `{data-dir}/skills-market` 目录——目录扫描等于无授权放行。

#### Scenario: 市场不可达时降级

- **WHEN** 市场服务宕机，用户调用 `luban_list_skills`
- **THEN** 返回仅含本地（global+user）技能的列表，附带 WARN 日志，工具调用本身成功

#### Scenario: 永不回退为目录扫描

- **WHEN** 市场拉取失败，但 `{data-dir}/skills-market` 下存在其他用户已同步的技能目录
- **THEN** 这些目录中的任何技能都不出现在列表中、不可被任何工具读取

### Requirement: Login JWT context pipeline

系统 SHALL 通过 `skillmarket.WithToken(ctx, token)` / `TokenFromContext(ctx)`（未导出 context key 类型）将 turn 发起请求的原始 Bearer token 注入 turn context；注入点为 `MessageStreamHandler.SendMessage`（与 trace_id / sessionID 注入同层），turn goroutine 的 detached context 继承该值。`AuthMiddleware` SHALL 保持现状不变（只发布 user_id，不保留 token）。

#### Scenario: 工具层可读取 token

- **WHEN** 一个 turn 中 agent 调用了需要 allowlist 的工具
- **THEN** 市场客户端从工具执行 ctx 中取到原始 JWT 并用于出站请求

#### Scenario: AuthMiddleware 不变

- **WHEN** 任意 API 请求通过鉴权中间件
- **THEN** gin context 上仍只新增 user_id（与 trace 回填），原始 token 不进入 gin context

### Requirement: Market disk layout and path safety

市场技能实体 SHALL 位于 `{data-dir}/skills-market/{market_user_id}/{skill_name}/`，由 operator 同步维护；路径首段的 user_id 是**市场侧**身份，blowball SHALL NOT 校验也不使用它（可见性判断完全委托市场服务）。API 返回的 `path` 拼接磁盘路径时 SHALL 执行 `filepath.Join(marketRoot, path)` 后 `filepath.Clean` 并断言结果仍位于 marketRoot 之下（拒绝 `..` 等逃逸；path 是半信任输入）。技能目录内的子路径访问 SHALL 复用 `skill.ValidateSubPath` 的目录内限制（拒绝绝对路径、`..` 逃逸、symlink 越界），与本地技能完全一致。

#### Scenario: 正常路径拼接

- **WHEN** allowlist 含 `path: "/019fef90-…/fund-promo-sentiment-v2"` 且该目录已在磁盘同步
- **THEN** 解析出的技能目录为 `{data-dir}/skills-market/019fef90-…/fund-promo-sentiment-v2`

#### Scenario: 逃逸路径被拒绝

- **WHEN** allowlist 中某条目的 `path` 为 `/../../etc` 或经 Clean 后逃逸出 marketRoot
- **THEN** 该条目被丢弃并记 WARN，任何工具不可经由它读取 marketRoot 之外的文件

#### Scenario: 子路径限定与本地技能一致

- **WHEN** 对市场技能调用读取类工具且子路径含 `..` 逃逸或指向目录外 symlink
- **THEN** 复用 `skill.ValidateSubPath` 拒绝，错误语义与本地技能相同

### Requirement: Local-first name conflict precedence

技能名解析顺序 SHALL 为 `user > global > market`：当市场技能名与本地技能（用户目录或全局目录）同名时，本地版本胜出；市场来源仅在本地 miss 后参与解析。

#### Scenario: 本地同名技能覆盖市场条目

- **WHEN** 用户目录存在技能 `foo`，市场 allowlist 亦含名为 `foo` 的技能
- **THEN** 列表与读取均解析到用户目录版本，市场条目不出现

### Requirement: Disk-sync lag tolerance

清单展示 SHALL 以 API 返回为准：磁盘尚未同步的市场技能照常出现在列表中（不因 stat 失败而隐藏）；读取类工具（read / list_skill_files / tree_skill）对磁盘缺失的市场技能 SHALL 返回 "directory not found" 类明确错误。

#### Scenario: 看得到读不到时明确报错

- **WHEN** allowlist 含技能 `bar` 但 `{data-dir}/skills-market` 下对应目录尚未同步，调用 `luban_read_skill(name="bar")`
- **THEN** 返回明确的 "directory not found" 错误，而非静默隐藏或 "skill not found"
