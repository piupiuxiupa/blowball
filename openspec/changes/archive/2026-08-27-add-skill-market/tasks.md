# Tasks: add-skill-market

## 1. 配置块

- [x] 1.1 `internal/config/config.go` 新增顶层 `SkillMarket` 配置块（`url`/`timeout`/`cache_ttl`，默认 `""`/`5s`/`60s`），接入 `${VAR}` 展开；启用态校验 `url` 为绝对 http(s) URL、两 duration 非负，违例 load 失败；`config.example.yaml` 增加注释示例块
- [x] 1.2 `internal/config` 单测覆盖：块省略=关闭零值、空 url=关闭、非法 url 拒绝、负 duration 拒绝、`${VAR}` 展开

## 2. skillmarket 包（客户端 + ctx 管道）

- [x] 2.1 新建 `internal/skillmarket`：`WithToken`/`TokenFromContext`（未导出 context key，仿 `agent.WithSessionID`）与 `Client`（共享 `*http.Client`、`GET url` + `Authorization: Bearer`、per-call deadline、解析 `{"skills":[...]}`、忽略 `role`）
- [x] 2.2 实现 per-user allowlist TTL 缓存（mutex 保护的 map，key=userID，TTL 来自配置；缓存只存 API 值不掺磁盘状态）与 fail-closed 语义（超时/非 200/解析失败 → WARN + 空 allowlist，绝无目录扫描回退）
- [x] 2.3 实现路径拼接与守卫：`Join(marketRoot, path)` + `Clean` + 前缀断言（逃逸条目丢弃 + WARN），导出 name→目录解析入口供 luban/executor 复用
- [x] 2.4 单测：httptest 覆盖成功解析、401/超时/非 200/坏 JSON 降级、TTL 窗口内单次出站、`../` 逃逸拒绝、空 token 行为

## 3. luban 工具接入

- [x] 3.1 `internal/tool/luban` 的 `Tools` 结构注入可选市场解析依赖（nil=现状不变）；名字解析扩展为 `user > global > market`，`readSkill`/`resolveSkillSubdir`/`listSkills` 三处共用同一 market fallback 函数
- [x] 3.2 `luban_list_skills` 合并市场条目（`location: "skill_market"`、description 取 API 值、本地优先、市场失败降级为仅本地）；`luban_read_skill`/`luban_list_skill_files`/`luban_tree_skill` 市场命中后复用 `skill.ValidateSubPath` 子路径限定，磁盘缺失返回 "directory not found"
- [x] 3.3 更新 `luban_read_skill` 工具描述（user > global > market 优先级）与 luban list 描述（市场来源说明）
- [x] 3.4 单测：市场条目列表/读取/列举/树、名字冲突本地胜、磁盘滞后报错、市场 nil 依赖时行为与现状逐字节一致

## 4. HTTP 层

- [x] 4.1 `MessageStreamHandler.SendMessage` 从请求头取原始 Bearer token，经 `skillmarket.WithToken` 注入 turn ctx（与 trace/sessionID 同层）；`AuthMiddleware` 保持不动并加守护测试（gin context 不出现 token）
- [x] 4.2 `GET /api/v1/skills` handler 合并市场条目（直接用请求头 token 调客户端，`location: "skill_market"`、本地优先、fail-closed 降级 HTTP 200）
- [x] 4.3 handler 单测：合并列表、冲突覆盖、市场挂掉仅本地返回

## 5. 运行时目录与沙箱

- [x] 5.1 `setupRuntime`：`MkdirAll({d}/skills-market)`（与 tools 同序列）；Landlock `roDirs` 加入该目录；确认不参与 shared-storage 健康检查
- [x] 5.2 `internal/tool/executor`：`Tools` 注入市场解析依赖（nil=不挂载）；`buildBwrapArgs` 按当前用户 allowlist 逐技能生成 `--ro-bind {d}/skills-market{path} /skills/market/{name}`（按名拍平、同名取首个、stat 守卫跳过磁盘缺失、空/失败 allowlist 零挂载）
- [x] 5.3 bash 工具描述补充 `/skills/market/{skill-name}/` 路径约定
- [x] 5.4 单测：bind 参数生成（授权/未授权/磁盘缺/市场失败四态）、描述文案、nil 依赖零改动

## 6. 装配与集成

- [x] 6.1 `cmd/blowball/serve.go`：构造 `skillmarket.Client` 并注入 luban 工具、executor、skills handler、SendMessage token 管道；api 与 agent 两角色均装配（api 角色仅 handler 用途）
- [x] 6.2 集成测试（`test/integration/`）：fake 市场服务覆盖 list 合并、读取第三来源、GET /skills 合并、fail-closed；`ValidateSubPath` 的 `EvalSymlinks` 在挂载/bind 场景（tmp 目录 symlink 布局模拟）行为验证
- [x] 6.3 `make lint && make test` 全绿；CLAUDE.md 增补 skill-market 段落（配置、目录、挂载约定、fail-closed 语义）
