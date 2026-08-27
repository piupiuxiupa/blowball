# Proposal: add-skill-market

## Why

技能目前只能来自 operator 手工放置的 `{data-dir}/skills/` 全局目录和用户自己 `luban_install_skill` 安装的目录,没有集中的"平台投放、按用户授权"的分发渠道。需要一个对接外部技能市场服务的只读技能来源:市场服务决定每个用户能看到哪些技能,技能实体由 operator 同步落到共享目录,blowball 严格按远端返回的清单放行读取——授权与内容分发分离,且授权对文件系统层(bash 沙箱)同样成立。

## What Changes

- 新增运行时目录 `{data-dir}/skills-market`(与 `skills`/`tools` 同级,operator 同步维护,内层结构 `{market_user_id}/{skill_name}/`),进程 Landlock 以只读覆盖。
- 新增配置块 `skill_market: {url, timeout, cache_ttl}`;块缺省或 `url` 为空 = 功能完全关闭(字节级现状行为)。
- 新增 `internal/skillmarket` 客户端:以发起会话用户的 login JWT 为凭证(`Authorization: Bearer`,市场服务从 token 解出用户身份)拉取该用户可访问的技能清单,per-user TTL 缓存,任何失败 fail-closed(市场技能不出现、读取被拒),绝不回退为扫描磁盘目录。
- 打通原始 JWT 的 ctx 管道:`MessageStreamHandler.SendMessage` 将请求头中的 raw token 注入 turn context(新 `skillmarket.WithToken`,未导出 key,仿 `agent.WithSessionID` 先例);`AuthMiddleware` 不动。
- luban 技能工具新增市场来源:名字解析顺序 `user > global > market`(本地优先);`luban_list_skills` 合并市场条目(`location: "skill_market"`);`luban_read_skill` / `luban_list_skill_files` / `luban_tree_skill` 在本地 miss 后按 allowlist 解析到 `{data-dir}/skills-market{path}`,子路径限定复用 `skill.ValidateSubPath`;磁盘同步滞后时 list 照常显示、读取报错。`luban_install_skill` 不变;`agents.<name>.skills` 系统 prompt 注入不变(市场技能靠工具发现,不自动进 prompt)。
- `GET /api/v1/skills`(api 角色)合并市场技能并标注 `location: "skill_market"`;handler 直接从请求头取 token,api 与 agent 两角色均构造市场客户端。
- bash 沙箱按 allowlist 逐技能挂载市场目录:每个已授权且磁盘存在的技能一条 `--ro-bind ... /skills/market/{name}`(按技能名拍平,对齐现有 `/skills/global` 约定),不整目录挂载(否则任意用户可通过 bash 读到其他用户的市场技能);stat-guard 跳过磁盘缺失条目;bash 工具描述补充路径约定。

## Capabilities

### New Capabilities

- `skill-market`: 远端授权的技能市场来源——配置块、JWT 拉取客户端、per-user allowlist TTL 缓存与 fail-closed 语义、`{data-dir}/skills-market` 磁盘布局、API 返回路径的拼接与逃逸防护、名字冲突的本地优先规则、JWT ctx 管道(`skillmarket.WithToken`)。

### Modified Capabilities

- `luban-skill-tools`: `luban_list_skills` 输出合并市场条目;`luban_read_skill` / `luban_list_skill_files` / `luban_tree_skill` 名字解析新增市场第三来源(user > global > market),磁盘滞后报错而非静默。
- `workspace-api`: Skills list 需求变更——`GET /api/v1/skills` 合并市场技能,市场条目标注 `skill_market` 来源。
- `executor-tools`: bash 沙箱新增按 allowlist 的逐技能市场只读挂载(`/skills/market/{name}`)、stat-guard 与工具描述中的路径约定。
- `sandbox-directory-configuration`: 进程 Landlock 只读目录集与启动目录创建新增 `{data-dir}/skills-market`。
- `service-roles`: 两角色共享的 `-d` 派生落盘目录枚举新增 `skills-market`(api 角色装配市场客户端用于 skills 列表端点)。

## Impact

- **代码**: `internal/config`(skill_market 块与校验)、新包 `internal/skillmarket`(客户端 + ctx key)、`internal/tool/luban`(名字解析第三来源 + list 合并)、`internal/tool/executor`(逐技能 bind + 描述)、`cmd/blowball/serve.go`(装配、目录创建、Landlock、两角色客户端构造)、`internal/handler`(token 注入 + skills handler 合并)。`skill.Loader` 与 `internal/middleware` 不动。
- **API**: `GET /api/v1/skills` 响应新增 `location: "skill_market"` 条目(向后兼容的增量);无新路由。
- **运维**: operator 需同步维护 `{data-dir}/skills-market`(多机部署各自挂载,同 `tools` 目录约定;不参与 shared-storage FUSE anchor);配置 `skill_market.url` 指向的市场服务必须能校验 blowball 的 JWT(共享 `jwt.secret` 域)。
- **依赖**: 无新第三方依赖(纯 stdlib HTTP,仿 `internal/memory` 形态)。
