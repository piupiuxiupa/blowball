## 1. 引擎拆分(xizhi 层)

- [x] 1.1 `internal/tool/xizhi/find.go`:把 `findRun` 的「校验」(validatePath/stat 根/参数枚举检查/pattern 编译/缺省值)与「执行」(引擎调用 + `buildFindResult` 排序分页)拆开;执行半导出为 `RunSearch(ctx, PreparedSearch) (SearchPage, error)` —— `PreparedSearch` 携带已校验的 `RelPath/AbsPath/Pattern(re)/EntryType/MaxDepth/IgnoreCase/IncludeHidden/HeadLimit/Offset`,`SearchPage` 为带类型页(entries `{Path, Type}` + `Total/Truncated/AppliedLimit/AppliedOffset`),不再返回 `any` 信封
- [x] 1.2 `findRun`/`FindEntries` 重组为「agent 校验 + RunSearch + 信封包装」,返回形状与错误文案逐字节不变;现有 `find_test.go` 全部保持绿(零行为变化的守门测试)
- [x] 1.3 确认两引擎 ctx 短路路径(fd `CommandContext` / Go walk `ctx.Err()`)在 `RunSearch` 透传 ctx 后仍生效,补一条 ctx 取消的单测

## 2. REST handler 与路由

- [x] 2.1 `internal/handler/workspace.go` 新增 `Search` 方法:query 参数解析(`pattern`/`type`/`path`/`max_depth`/`ignore_case`/`include_hidden`/`head_limit`/`offset`,缺省 any/根/不限/true/false/200/0);`type=file|dir|any` → xizhi 内部 `directory` 映射;非法参数 400
- [x] 2.2 根校验:空/空白 `path` → 工作区根,否则 `xizhi.ValidatePathAllowReserved`(越界 403;不存在 200 空;`os.Stat` 为普通文件 400 `INVALID_PATH`);`pattern` 经 `regexp.QuoteMeta` 转义为字面量子串
- [x] 2.3 调 `RunSearch`(`context.WithTimeout`,10s 常量):超时 500 `SEARCH_TIMEOUT`;成功后对返回页逐条 `os.Stat` 组装 `{path, name, type("file"|"dir"), size, update_time(RFC3339)}`,stat 失败保留条目降级 `size:0`/`update_time:""`;响应回显用户原始 `pattern`(非转义串)与 `total/truncated/applied_limit/applied_offset`
- [x] 2.4 `internal/handler/router.go`:`RouteDeps` 加 `WorkspaceSearch`;`RegisterAPIRoutes` 注册 `authed.GET("/workspace/search", deps.WorkspaceSearch)`;`cmd/blowball/serve.go` `wireAPI` 接线

## 3. 测试

- [x] 3.1 handler 单测(`internal/handler/workspace_test.go`):子串命中含特殊字符(`report(1)`)、空 pattern+type 枚举、ignore_case 缺省 true/显式 false、hidden 缺省排除与 `.blowball` 根可搜、越界 403、缺根 200 空、文件根 400、分页窗口与 truncated、TOCTOU 降级、非法参数 400、未鉴权 401
- [x] 3.2 引擎拆分回归:`internal/tool/xizhi/find_test.go` 现有用例不动且全绿;`test/integration/role_ownership_test.go` api 分区断言加 `GET /workspace/search`(agent 分区不含)
- [x] 3.3 `make test` + `make lint` 全绿

## 4. 契约与文档

- [x] 4.1 `api/openapi.yaml`:`/api/v1/workspace/search` 的 `get` 操作(query 参数、响应 schema、401/403/400/500 错误);复制到 `blowball-frontend/` 并 `npm run generate-api` 再生成
- [x] 4.2 `CLAUDE.md`:HTTP 路由表加一行;workspace file routing 小节说明 search 端点在 catch-all 之外的静态分叉(与 upload 同理)
