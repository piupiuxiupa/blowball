# Design: add-skill-market

## Context

技能发现现状(`internal/tool/skill/skill.go`):`Loader` 是**纯磁盘**的两源合并——全局 `{data-dir}/skills/`(Location="global")与用户 `data/{userID}/skills/`(Location="user"),user 覆盖 global。luban 工具的名字解析收敛在两处:`loader.Read/ReadPath`(read_skill)与 `resolveSkillSubdir`(list_skill_files / tree_skill),均先 `SkillDir(name, userID)` 再 `ValidateSubPath` 做目录内子路径限定(拒绝绝对路径、`..`、symlink 逃逸)。

与本需求相关的四个既有事实:

1. **原始 JWT 在验证后被丢弃**——`authenticate()`(`internal/middleware/auth.go:71`)只把 user_id 发布到 gin context。
2. **ctx values 已有从 HTTP 层流到工具闭包的完整先例**:`SendMessage` 注入 trace/sessionID → `Orchestrator.Handle` 注入 `skill.WithUserID`(`internal/agent/orchestrator.go:451`)→ 工具闭包 `skill.UserIDFromContext(ctx)` 读取。
3. **bwrap 已绑全局技能**:`--ro-bind globalSkillsDir /skills/global`(`internal/tool/executor/bwrap.go:81`),`/skills/` 命名空间预留了多来源空间;`buildBwrapArgs` 是**每次执行时**构建的,可按调用者注入 per-user 挂载。
4. **`{d}/tools` 是"operator 维护、只读、同级"目录的完整先例**:启动 MkdirAll、Landlock RO、内容 operator 直接放盘。

市场服务的契约:接收 blowball 的 login JWT(`Authorization: Bearer`),从 token 解出用户身份,只返回该用户可访问的技能;响应 `{"skills":[{slug,name,description,role,path}]}`,`path` 形如 `/019fef90-ee91-702f-b85e-e8f71973af44/fund-promo-sentiment-v2`(首段是**市场侧** user_id,不是 blowball 的);`role` 忽略不过滤。

## Goals / Non-Goals

**Goals:**

- 技能的第三来源"市场":授权由远端 API 决定,内容由 operator 同步到 `{data-dir}/skills-market`。
- allowlist 严格性对**所有**工具成立——luban 读工具与 bash 沙箱都只能碰接口返回的路径。
- 零行为变化的安全开关:不配置 `skill_market` 时字节级等于现状。

**Non-Goals:**

- 不做市场的安装/卸载/购买流转(`luban_install_skill` 仍只装本地;安装态由市场平台管理,blowball 只读)。
- 不把市场技能注入系统 prompt(`agents.<name>.skills` 语义不变;市场技能靠 `luban_list_skills` 被模型发现)。
- 不动 `skill.Loader`(保持纯磁盘发现)与 `AuthMiddleware`(保持只发布 user_id)。
- 不校验市场路径首段的 user_id(可见性判断完全委托市场服务;blowball 无法知道市场侧身份映射)。
- 不引入市场清单的持久化/事件推送(仅 TTL 缓存)。

## Decisions

### D1. JWT 管道:handler 注入 + 消费者包持有 ctx key

新包 `internal/skillmarket` 提供 `WithToken(ctx, tok)` / `TokenFromContext(ctx)`(未导出 key 类型,仿 `agent.WithSessionID` 先例)。`MessageStreamHandler.SendMessage` 从请求头取 raw Bearer token,与 trace/sessionID 一同注入 turn ctx;turn goroutine 的 detached ctx 继承 values,工具调用时可读。

- 备选一:`AuthMiddleware` 把 token 发布到 gin context——被否:gin 层多一个存放点,且 query-token 中间件路径会引入歧义;handler 注入已足够、触碰面最小。
- 备选二:send-time 预取清单快照进 ctx(不传 token)——被否:handler 需为没用 luban 的 turn 白付一次请求;token 用未导出 key 隔离后,传递本身不构成暴露面。
- 已知边界:turn 生命期可能超过 JWT TTL;此时缓存过期后的重新拉取会 401 → 按 fail-closed 降级(WARN + 市场技能不可用),与整体故障语义一致。

### D2. 客户端:惰性拉取 + per-user TTL 缓存 + fail-closed

`internal/skillmarket.Client`,形态仿 `internal/memory.Service`:一个共享 `*http.Client`、per-call deadline(`timeout`,默认 5s)、per-user allowlist 缓存(`cache_ttl`,默认 60s,key=userID,mutex 保护的 map,基数=用户数)。首次需要 allowlist 的调用(luban 工具或 bash)触发拉取,之后 TTL 内零成本。

- 备选:turn 开始预取(仿 memory recall)——被否:handler 又多一个依赖,没配 luban/executor 的 turn 白付请求;惰性 + 缓存在 60s TTL 下出站次数与预取同量级。
- **fail-closed 语义**:URL 挂/超时/非 200/解析失败 → WARN + 返回空清单;市场技能从 list 消失、读取/挂载被拒("not found")。**绝不**回退为扫描 `{d}/skills-market` 目录——那等于无授权放行。撤销延迟 = TTL(已确认 60s 可接受)。
- 缓存条目持有 API 的 name/description/path 原值;磁盘是否落盘在**使用点**检查(list 直接展示,read/bind 时报错/跳过),缓存不掺磁盘状态。

### D3. 名字解析:市场是 luban 层的第三来源,优先级 user > global > market

luban 工具解析链在本地 `loader.SkillDir` miss 后查 allowlist:`name → {d}/skills-market{path}`。冲突时本地优先(已确认)。

- 备选:把市场做进 `Loader` 第三源——被否:`Loader` 无网络依赖是刻意纯度(测试与复用都依赖它),且 list 的 market 条目元数据来自 API 而非磁盘扫描,数据源性质不同。
- 实现:在 luban 包内引入一个小解析函数(输入 name + allowlist,输出目录),`readSkill` / `resolveSkillSubdir` / `listSkills` 三个调用点共用;`Tools` 结构持有可选的市场解析依赖(nil = 功能关闭,行为不变)。

### D4. 路径安全:root 断言 + 子路径复用 ValidateSubPath

`filepath.Join(marketRoot, path)` 后 `filepath.Clean`,断言结果仍以 `marketRoot` 为前缀(API path 是半信任输入,防 `../` 逃逸;Windows 盘符类不需要考虑——仅 POSIX 部署)。技能内子路径一律复用 `skill.ValidateSubPath`(绝对路径、`..`、symlink 逃逸全拒)。

- 风险点:`ValidateSubPath` 内部 `EvalSymlinks` 在 JuiceFS/容器 bind 挂载上的行为需集成测试覆盖(挂载点自身的 symlink 解析);若挂载层返回不一致结果,降级路径是读前 `os.Stat` 兜底(与 stat-guard 同族),但先以测试实证为准。

### D5. 磁盘同步滞后:list 以 API 为准,读取报错

list 直接展示 API 清单(API 是可见性唯一事实);read/list_files/tree 对磁盘缺失的技能返回 "directory not found"(已确认)。不同步到"stat 后过滤"——静默隐藏会掩盖运维同步问题。

### D6. 配置块:`skill_market: {url, timeout, cache_ttl}`,url 非空即启用

无独立 `enabled` 开关(没有"配置了但要关掉"的场景;要关就删块)。缺省块/空 url = 功能关闭 = 字节级现状。所有字符串值走既有 `${VAR}` 展开;`url` 启用时必须是绝对 http(s);`timeout`/`cache_ttl` 正 duration,负值 load 失败。放进 `config.example.yaml` 注释块。

### D7. 运行时目录与 Landlock

`setupRuntime`:`MkdirAll({d}/skills-market)`(空目录无害,同 tools);Landlock 进程 RO 目录集加入该目录(与 `{d}/tools` 并列;agent 不应写市场)。bwrap 以 RO bind 消费同一目录,宿主侧只需读权限。

### D8. bwrap 沙箱:按 allowlist 逐技能挂载到 `/skills/market/{name}`

- **不整目录挂载**:整挂 `skills-market → /skills/market` 会让任意用户的 bash 读到**其他用户**的市场技能,直接违反"严格限制读取路径只能是接口返回的那些路径"。bash 也是工具。
- 每次 bash 调用时(`buildBwrapArgs` 按调用构建),经 TTL 缓存解析该用户 allowlist,对每个**已授权且磁盘上存在**的技能生成一条:`--ro-bind {d}/skills-market/{market_uid}/{name} /skills/market/{name}`。
- **按技能名拍平**目标路径(不保留 `{market_uid}` 层级):agent 从 `luban_list_skills` 拿到技能名即可推导脚本路径(市场技能脚本在 `/skills/market/{skill-name}/`),无需知道市场侧 user_id;name 本就是 luban 解析主键,不引入新歧义(同名时取 allowlist 首个)。
- **stat-guard**(复用 `existingDirs` 先例):磁盘缺失的条目跳过 bind 而非让 bwrap 启动失败——bash 照常可用,跑到该技能脚本时才报 not-found,与 D5 语义一致。allowlist 为空/拉取失败 → 零市场挂载,fail-closed。
- 装配:`internal/tool/executor` 的 `Tools` 结构注入可选的市场解析依赖(nil = 不挂,现状不变);bash 工具描述补一句路径约定。

### D9. `GET /api/v1/skills` 合并市场条目

handler 直接从请求头取 token 调客户端(该端点不需要 ctx 管道),合并输出,市场条目 `location: "skill_market"`、description 用 API 值、名字冲突本地优先。api 与 agent 两角色均构造客户端(api 角色因此新增对 `internal/skillmarket` 的装配依赖,仍不触 LLM/orchestrator 层)。

### D10. 超时组合

市场请求自身受 `skill_market.timeout`(默认 5s)约束;`tools.timeouts` 中央机制可再兜底 luban 工具总时长(市场 fetch 计入其中),建议 operator 配置时给 luban 工具留出市场预算。

## Risks / Trade-offs

- [市场服务故障拖慢 luban/bash 首次调用(缓存 miss 时 +timeout)] → per-call deadline 5s + TTL 缓存 + WARN-only;`tools.timeouts` 可再收紧。
- [TTL 窗口内撤销不即时] → 已确认 60s 可接受;清单语义是"安装态"而非会话级许可。
- [API 返回恶意 path 逃逸] → D4 root 断言;path 仅在 join 后使用,不进任何模型可见输出。
- [磁盘同步滞后造成"看得到读不到"] → D5 明确报错;运维以市场平台同步状态为准。
- [EvalSymlinks 在挂载 FS 上行为不定] → 集成测试覆盖;异常时以 os.Stat 兜底(实现期验证后再定)。
- [bash 整挂的越权读(若未来有人改成整挂)] → spec 层写死"逐技能挂载"为 REQUIREMENT,防回归。
- [JWT 过期的长 turn] → fail-closed 降级,不影响 turn 其他工具。

## Migration Plan

1. 部署市场服务(共享 `jwt.secret` 域,能校验 login JWT 并返回清单)。
2. operator 在每台 blowball 主机挂载/同步 `{d}/skills-market`(多机各自挂载,同 `tools` 约定)。
3. `config.yaml` 增加 `skill_market:` 块;重启进程。
4. 回滚:删除 `skill_market:` 块重启即回到字节级现状;`{d}/skills-market` 目录留存无害。

## Open Questions

无——探索阶段已全部锁定(D1–D10 对应的十个决策均经确认)。实现期唯一的实证项:D4 的 EvalSymlinks 挂载行为测试。
