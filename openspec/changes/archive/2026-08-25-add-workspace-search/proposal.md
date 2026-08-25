# Proposal: add-workspace-search

## Why

前端目前定位工作区文件只能通过 `GET /api/v1/workspace/files?path=` 一层层展开目录树,没有跨目录的名字搜索能力。agent 侧早已有对等能力(`xizhi_find`:fd/纯 Go 双引擎、basename 匹配、类型过滤、分页),但那是模型工具,REST 层拿不到。需要把这套引擎以 HTTP 接口的形式暴露给前端(文件选择器/搜索面板类 UI)。

## What Changes

- 新增 `GET /api/v1/workspace/search`(JWT 鉴权,`api` 角色路由分区,`api`/`all` 角色注册)。路由不能是 `/workspace/files/search`——会被现有 `GET /workspace/files/*path` catch-all 吞掉,gin 也不允许静态段与通配符同节点并存;`/workspace/search` 在 `/workspace/{search,files}` 节点分叉,与 `upload` 同款姿势。
- 请求参数(全部 query):`pattern`(可选,**子串语义**——服务端 `QuoteMeta` 转义为字面量再匹配,匹配条目名 basename,缺省匹配一切)、`type`(`file`/`dir`/`any`,缺省 `any`;值域对齐 List 的 `"dir"` 而非 xizhi 内部的 `"directory"`)、`path`(搜索根,相对工作区根,缺省为根;经 `ValidatePathAllowReserved` 校验——与 List 一致,`.blowball` 保留命名空间允许作为搜索根)、`max_depth`(非负整数,缺省不限)、`ignore_case`(缺省 **true**,人类搜索约定;与 xizhi_find 的 false 缺省有意分叉)、`include_hidden`(缺省 false,`.blowball` 因此默认不可见,与 List 一致)、`head_limit`/`offset`(缺省 200/0)。
- 响应 200:`{total, truncated, applied_limit, applied_offset, entries: [{path, name, type, size, update_time}]}`。`path` 工作区相对、`name` 为 basename、`type` 为 `"file"`/`"dir"`、`update_time` RFC3339(与 List 的 fileEntry 字段约定一致)。`size`/`update_time` **仅对返回页**逐条 `os.Stat`(JuiceFS 共享存储下 stat 是网络往返,不做全量命中 stat);walk 与 stat 之间条目消失时保留条目、以 `size: 0` + `update_time: ""` 降级,不静默跳过(否则本页条数与 `total` 对不上,分页失效)。
- entries 按路径字典序稳定排序——分页的骨架,**不做相关性排序**;排序/重排留给前端。
- 整个搜索(遍历 + stat)受服务端总超时约束(固定常量,约 10s);超时返回 500 `SEARCH_TIMEOUT`。fd 与纯 Go 引擎均已支持 ctx 取消。
- 引擎复用:`findRun` 拆为「校验」与「执行」两半——agent 入口 `findRun`/`FindEntries` 行为零变化(继续走 `ValidatePath`、拒绝 `.blowball`);新导出一个吃 ctx、吃已校验入参的执行入口(`PreparedSearch` + `RunSearch`),REST handler 自己按 AllowReserved 约定校验后调用。
- `api/openapi.yaml` 补 `/api/v1/workspace/search` 的 `get` 操作;前端仓库随后重新生成 API 类型。
- 不做:文件内容搜索(`xizhi_grep` 以后可加兄弟端点)、相关性排序、正则 pattern。

## Capabilities

### New Capabilities

(无)

### Modified Capabilities

- `workspace-api`: 新增 "Search workspace entries" 需求 —— 端点参数契约(子串 pattern、type 值域、AllowReserved 根校验、ignore_case 缺省 true)、响应形状(含 stat 仅返回页与 TOCTOU 降级语义)、字典序分页、超时行为、错误映射(403 越界 / 200 空缺根 / 400 非目录根与非法参数)。
- `api-server`: "API routing" 需求的 Workspace routes 清单加入 `GET /api/v1/workspace/search`,并新增该路由的鉴权与角色注册场景(`agent` 角色不注册)。

## Impact

- `internal/tool/xizhi/find.go` — `findRun` 拆分:抽出吃 ctx、吃已校验 `findInput` 的执行入口并导出(`PreparedSearch`/`RunSearch` 形态);`FindEntries`/`findRun` agent 路径零行为变化。
- `internal/handler/workspace.go` — `WorkspaceHandler` 新增 `Search` 方法:参数解析、`ValidatePathAllowReserved` 根校验、`QuoteMeta` 子串转义、type/file→dir 值域映射、返回页 stat 与 TOCTOU 降级、10s 超时。
- `internal/handler/router.go` — `RouteDeps` 新增 `WorkspaceSearch`;`RegisterAPIRoutes` 注册 `GET /workspace/search`。
- `cmd/blowball/serve.go` — `wireAPI` 接线新 handler 方法。
- `api/openapi.yaml` — 新增 `get` 操作;`blowball-frontend` 同步 `npm run generate-api` 再生成。
- `test/integration/role_ownership_test.go`、`internal/handler/router_test.go` — api 分区路由断言加入新端点。
- `CLAUDE.md` — 路由表与 workspace file routing 小节补一行。
