## 1. find 引擎与实现

- [x] 1.1 新增 `internal/tool/xizhi/find.go`:`findInput`/`findResult`/`findEntry` 类型与 `FindEntries` 入口(参数校验:path 目录校验、type 枚举、max_depth 正整数、pattern 编译——`ignore_case` 时 `(?i)` 前缀;`ValidatePath` 安全校验),返回形状 `{path, pattern, type, total, truncated, applied_limit, applied_offset, entries:[{path,type}]}`
- [x] 1.2 新增 `internal/tool/xizhi/find_go.go` 纯 Go 引擎:`filepath.WalkDir` + basename regex + 类型/深度/hidden 过滤,不跟 symlink,不可读条目跳过不中断(对齐 `goGrepEngine` 跳过语义),`maxFindCollect`(10000)收集上限
- [x] 1.3 新增 `internal/tool/xizhi/find_fd.go` fd 引擎:`sync.OnceValue` 解析 `exec.LookPath("fd")` → 回退 `fdfind` → Go 引擎(仿 `defaultEngine` 先例);flags 固定化(`--no-ignore` 恒开、默认 `-s --case-sensitive` / `ignore_case` 时 `-i`、`--hidden` 仅 include_hidden、`-t f`/`-t d`、`-d N`)、`cmd.Dir = absPath`、按行解析输出(尾 `/` 判 directory);exit 0 = 成功(无匹配也是 0),非 0 取 stderr 报错
- [x] 1.4 共享 mapper:entries 按路径字典序排序(fd 并行遍历无序/Go WalkDir 目录序,排序保证一致)、`total`/`truncated`/`applied_limit`/`applied_offset` 分页窗口(head_limit 默认 200、offset 默认 0)、collect cap 置 truncated(对齐 `buildGrepResult` 形状)

## 2. 注册替换与周边换名

- [x] 2.1 `internal/tool/xizhi/register.go`:`NameFind = "xizhi_find"` 常量;`xizhi_glob_files` 注册块替换为 `xizhi_find`(description 含 regex 示例 `\.go$`、basename 匹配说明、type/max_depth/分页引导、pattern 为 regex 非 glob 的显式提示);删除 `NameGlobFiles` 与 `internal/tool/xizhi/glob.go`
- [x] 2.2 `internal/config/config.go`:`XizhiConfig.GlobFiles`(yaml `glob_files`)→ `Find`(yaml `find`);校验层探测原始 `tools.xizhi.glob_files` key,存在即 fail-fast,错误 `tools.xizhi.glob_files was removed; use tools.xizhi.find (tool xizhi_find)`
- [x] 2.3 `internal/agent/orchestrator.go` `isXizhiTool`:`NameGlobFiles` → `NameFind`
- [x] 2.4 `internal/tool/executor/register.go` bash steering 文案:`find`→`xizhi_glob_files` 改指 `xizhi_find`
- [x] 2.5 `config.example.yaml`:两处 `agents.*.tools` 列表、`tools.xizhi.glob_files` 开关块、`tools.timeouts` 示例 key 同步改名

## 3. 测试

- [x] 3.1 Go 引擎单测:regex 名字匹配、type 过滤(file/directory/any)、空 pattern 匹配一切、basename 不匹配路径、ignore_case、include_hidden、max_depth、path 默认根/非目录报错/`.blowball` 拒绝、非法 regex 报错、分页窗口与 truncated、collect cap
- [x] 3.2 fd 引擎单测(装有 fd 的环境跑,无 fd 跳过):同参数输出与 Go 引擎逐字段一致(含排序、分页、hidden、type、depth);`fdfind` 别名解析
- [x] 3.3 config 迁移测试:`tools.xizhi.find.enabled` 开关注册;`tools.xizhi.glob_files` 出现时加载报错且错误文案含迁移指引;`agents.*.tools` 遗留 `xizhi_glob_files` 经 `ToolsFor` 报 unknown tools
- [x] 3.4 更新/删除引用 `xizhi_glob_files`、`NameGlobFiles`、`GlobFiles` 的既有测试
- [x] 3.5 `make test` + `make lint` 全绿

## 4. 文档

- [x] 4.1 CLAUDE.md:工具族清单 `xizhi_glob_files` → `xizhi_find`(含新参数/返回形状/双引擎说明)、config 注释处同步
- [x] 4.2 `openspec/specs/xizhi-glob-files` 归档时随 REMOVED 移除;`xizhi-find-files` 成为正式 capability(归档流程处理,本 change 无需动作,仅提醒)
