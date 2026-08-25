# Design: add-workspace-search

## Context

前端需要一个跨目录的文件/目录名字搜索接口。后端 `internal/tool/xizhi` 已有完整的双引擎查找实现(`xizhi_find`):fd shell-out + 纯 Go 兜底、basename 匹配、类型/深度/hidden 过滤、字典序 + 分页 + 10000 条收集封顶,且两引擎逐字段一致。本变更是"把现成引擎以 REST 形态暴露",不是新写搜索引擎。

## Goals / Non-Goals

**Goals**

- `GET /api/v1/workspace/search`:按名字(basename)子串搜索工作区文件与目录,返回带 `size`/`update_time` 的分页结果。
- 引擎层零重复:REST 路径与 agent 路径共用同一对引擎与 mapper。
- agent 侧行为零变化(`xizhi_find` 的参数、校验、返回形状都不动)。

**Non-Goals**

- 文件内容搜索(`xizhi_grep` 能力,以后可加兄弟端点)。
- 相关性排序(字典序是分页骨架;basename 优先、最近修改加权等留给前端在一页内自排)。
- 正则 pattern(前端要正则时后续可加 `mode` 参数,本期子串)。
- 新配置项(超时是固定实现常量,不进 `config.yaml`)。

## Decisions

### D1. 路由是 `/workspace/search`,不是 `/workspace/files/search`

gin(httprouter)不允许静态段与 `*path` 通配符同节点并存,且 `GET /workspace/files/*path` catch-all 会把 `/workspace/files/search` 当作 `path="/search"` 吞掉。`/workspace/search` 在 `/workspace/{search,files,upload}` 节点静态分叉,是 `upload` 已验证过的姿势。挂在 `api` 分区(`RegisterAPIRoutes`)——纯读、无 agent 依赖,`wireAPI` 已构造 `WorkspaceHandler`。

### D2. `findRun` 拆为「校验」+「执行」,handler 走 AllowReserved 校验

```
handler (REST 层)                        xizhi (引擎层)
──────────────────────────               ──────────────────────────
参数解析 + ValidatePathAllowReserved
        │
        ▼
PreparedSearch{absPath, ...} ──────────▶ RunSearch(ctx, prepared)
                                           ├─ fd 引擎 ▸ 纯 Go 兜底
    findRun (agent 入口,ValidatePath      ├─ 字典序 + 分页 + 封顶
    拒 .blowball)──────────────────────▶  └─ 返回 typed 页(条目路径+类型)
        │
        ▼
对返回页逐条 os.Stat
        │
        ▼
组装 {path, name, type, size, update_time}
```

现状 `FindEntries` 硬编码 `context.Background()`,校验(`validatePath`,agent 版,拒 `.blowball`)与引擎执行耦合在 `findRun` 里。拆分后:

- 导出 `RunSearch(ctx, PreparedSearch)`,吃已校验入参、吃 ctx,返回**带类型的分页页**(`entries []{Path, Type}` + `total/truncated/appliedLimit/appliedOffset`),不返回 `any` 信封——信封渲染留在 agent 路径(`findRun` 把 typed 页包成现有 `{path, pattern, ...}` 形状),REST handler 自己组装响应 DTO(两边形状本来就不同:REST 有 `name/size/update_time`、type 用 `"dir"`)。
- agent 入口 `findRun` = agent 校验 + `RunSearch` + 信封包装,行为零变化。
- REST 侧校验用 `ValidatePathAllowReserved`(与 `List` 一致):`.blowball` 可以作为搜索根,用户能搜到自己的 `.blowball/skills/`——这正是 workspace.go:36 注释声明的设计意图(agent 被挡、用户可管理自己的应用状态)。

### D3. pattern 是子串,服务端 `QuoteMeta`

给人用的搜索框,不是给模型用的。`report(1).md`、`a+b` 这类输入按正则会炸或语义漂移;服务端 `regexp.QuoteMeta(input)` 后作为字面量匹配 basename,fd 引擎与 Go 引擎都拿正则语义,转义后的串两边通用,引擎无需改动。响应中回显的 `pattern` 是**用户原始输入**,不是转义后的串。空/纯空白 pattern = 匹配一切(与 xizhi_find 一致,天然支持"纯类型过滤枚举")。

### D4. `ignore_case` 缺省 true(与 xizhi_find 的 false 有意分叉)

人搜文件名几乎总是想忽略大小写;模型侧 xizhi_find 缺省大小写敏感是精确性优先。分叉只在 handler 的参数缺省层,引擎的 `ignoreCase` 传参不变。

### D5. type 值域用 `file`/`dir`/`any`,双向映射

REST 响应里 `List` 一直输出 `"dir"`(`fileType()`),前端类型判断已按此写死;搜索端点请求与响应统一 `file|dir|any`,handler 内部映射到 xizhi 的 `findTypeDirectory`。不引入 `"directory"` 第三种拼写给前端。

### D6. stat 仅返回页;TOCTOU 降级不静默跳过

引擎返回的页(≤ `head_limit`,缺省 200)才逐条 `os.Stat`。不做全量命中 stat——`storage.workspace.backend: shared`(JuiceFS)下 stat 是网络往返,200 次可接受、10000 次不可接受。

walk 与 stat 之间条目被删:保留条目、`size: 0` + `update_time: ""` 降级。对照 `List` 的静默跳过(`e.Info()` 失败 `continue`):搜索场景下跳过会让本页条数 < `applied_limit` 而 `total` 不变,前端翻页逻辑失效;降级保分页计数诚实。两引擎本就跳过 symlink,不会 stat 到悬空链接。

### D7. 字典序保留为分页骨架,不做相关性排序

分页必须有稳定全序,否则翻页漏/重。`buildFindResult` 现有按路径字典序排序即为此骨架,原样保留。"不做排序"指不叠加相关性逻辑(basename 命中优先、浅路径优先等),前端拿一页自己重排。

### D8. 10s 总超时(固定常量),500 `SEARCH_TIMEOUT`

`context.WithTimeout(requestCtx, 10s)` 覆盖遍历 + stat 全程:fd 引擎走 `exec.CommandContext`,Go 引擎逐条 `ctx.Err()` 短路,均已支持。超时返回 500 + code `SEARCH_TIMEOUT`(沿用 `errorBody(code, msg)` 约定),前端可提示"缩小搜索范围"。不选 504——语义是内部计算超时,不是网关上游。不做成配置——本期无运维调参需求,有再说。

## Error Mapping

| 情形 | 响应 | 对照 |
|------|------|------|
| 搜索根越界(绝对路径/`..`/symlink 逃逸) | 403 `FORBIDDEN` | 与 List 一致 |
| 搜索根不存在 | 200 + 空 entries(`total: 0`) | 与 List 一致 |
| 搜索根是普通文件 | 400 `INVALID_PATH` | List 此情形是 500(ReadDir ENOTDIR),搜索显式改对 |
| `type` 非法 / `max_depth` 负 / `head_limit`/`offset` 非法整数 | 400 `INVALID_REQUEST` | 参数级错误 |
| 未鉴权 | 401 | AuthMiddleware |
| 超时 | 500 `SEARCH_TIMEOUT` | D8 |

`head_limit` 上限:与引擎缺省对齐,超大方差无意义,clamp 到 200?——**不 clamp**,`applied_limit` 回显实际生效值,上限交给实现常量(与 xizhi_find 的处理一致,缺省 200,过大值由收集封顶自然约束)。

## Risks / Trade-offs

- **fd 子进程在 api 角色下首次可用性**:fd/Go 双引擎本就为无 fd 环境兜底,`agent` 角色生产上已验证 fd 可在 Landlock 下执行(`/usr` 只读基线),api 角色同一进程级 Landlock 策略,无新增面。
- **大 workspace 全量 walk**:pattern 为空 + 不限深度时等于全树枚举,靠 10000 收集封顶 + 10s 超时兜底;`truncated` 如实告知。
- **共享存储延迟**:JuiceFS 上 walk 较慢,10s 超时可能对超大工作区偏紧——超时是显式错误而非挂死,可接受;真不够再调常量或进配置。
- **与 `List` 的 500-vs-400 分叉**(非目录根):有意为之,不为对齐而对齐,spec 里写明即可。

## Migration Plan

纯新增端点 + 引擎内部拆分,无 schema、无配置、无部署变更。前端拿到新 openapi.yaml 后 `npm run generate-api` 再生成即可;不调用新端点的前端版本完全无感。

## Open Questions

(无——子串 pattern、ignore_case true 缺省两项按探索结论定稿,如需翻转为正则/大小写敏感,改 D3/D4 一处参数缺省即可。)
