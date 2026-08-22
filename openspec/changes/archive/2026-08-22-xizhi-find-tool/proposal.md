## Why

`xizhi_glob_files` 是 xizhi 工具族里最弱的查找工具:只认 doublestar glob(模型写 regex 会静默失败)、`matches[]` 是纯路径字符串(分不清文件还是目录)、无分页(`**/*` 全量返回)、无类型过滤。模型"按名字找文件/目录"是高频操作,目前只能用 tree(深度受限)或 grep 的 `files_with_matches`(先要知道内容)。以 fd 的查找语义重做这个工具:regex 匹配、类型可见/可过滤、分页可控。

同时,mcp-call-result-spill 落盘文件目录(`tmp/mcp-outputs/{server}/`)里堆积多轮结果后,模型需要一个按名字定位 spill 文件的工具——`xizhi_find` 恰好补位。

## What Changes

- 新增工具 `xizhi_find`:regex(默认匹配条目名/basename,fd 默认语义)递归查找工作空间内的文件**和目录**,支持 `type`(`file`/`directory`/`any`,默认 `any`)、`max_depth`、`ignore_case`、`include_hidden`、`head_limit`/`offset` 分页;返回 `{path, pattern, total, truncated, applied_limit, applied_offset, entries: [{path, type}]}`。
- 双引擎仿 rg 先例:系统装有 `fd`(含 Debian 系 `fdfind`)则 shell out,否则纯 Go fallback;共享 mapper 归一化排序/分页/truncated,两引擎输出逐字段一致。
- **BREAKING**:删除 `xizhi_glob_files` 工具。
  - config 迁移三处:`tools.xizhi.glob_files` key → `tools.xizhi.find`(旧 key 在加载时以指向迁移的错误拒绝);`agents.<name>.tools` 列表中的 `xizhi_glob_files` → `xizhi_find`(遗留旧名经工具注册表未知名检查在启动时 fail-fast);`tools.timeouts` 的 key 同步更名(旧 key 无害但永不匹配)。
- 周边文案/接线同步:bash 工具 steering 文案中 `find`→`xizhi_glob_files` 改指 `xizhi_find`;orchestrator 的 `isXizhiTool` 名单换名;grep description 中提及 glob 工具处核对。

## Capabilities

### New Capabilities

- `xizhi-find-files`: `xizhi_find` 工作空间条目查找工具——regex 名字匹配、文件/目录类型可见与过滤、深度限制、分页、fd/Go 双引擎一致性。

### Modified Capabilities

- `xizhi-glob-files`: 整个 capability 随工具删除而 REMOVED(含迁移指引到 `xizhi_find`)。

## Impact

- 代码:`internal/tool/xizhi/`(新增 find.go/find_engine、register.go 注册替换、glob.go 删除)、`internal/config/config.go`(`GlobFiles` → `Find` 字段 + 旧 key 拒绝)、`internal/agent/orchestrator.go`(`isXizhiTool`)、`internal/tool/executor/register.go`(steering 文案)。
- 部署迁移:config.yaml 三处改名(见 What Changes);无 DB/HTTP API/前端变更(工具名仅模型可见)。
- 可选运行时依赖:系统 `fd`/`fdfind`(缺失时纯 Go fallback,行为一致仅性能差异)。
