## Why

`xizhi_grep` 的 `path` 参数当前只接受目录:`grepRun` 对解析后的路径显式 `os.Stat` 并拒绝非目录(`internal/tool/xizhi/grep_engine.go` 的 `is not a directory` 检查)。而 mcp-call-result-spill 落盘的结果是**单文件**(`tmp/mcp-outputs/{server}/{tool}-{trace}-{seq}.txt`),其返回信封 hint 引导模型"用 xizhi_grep (regex) 读回"——模型照做即报 `xizhi grep: "tmp/mcp-outputs/..." is not a directory`,注定失败的第一次尝试,每次 spill 读回都浪费一轮工具调用 + 错误往返。大文件里正则捞几行恰是 spill 场景最需要的能力。

## What Changes

- `grepRun` 的 stat 分支放宽:路径是目录照旧;是**普通文件**则进入单文件搜索模式(仅搜索该文件)。
- rg 引擎适配单文件模式:`cmd.Dir` 改为 `filepath.Dir(absPath)`,rg 搜索目标从硬编码的 `"."` 改为文件名;`--json` 输出的 path 天然是 basename,与目录模式形状一致。
- Go 引擎适配:`filepath.WalkDir` 对单文件天然工作,仅需豁免"根文件自身"的 hidden-name 跳过(当前 `grep.go` 的文件级 hidden 过滤会把以 `.` 开头的根文件跳过)。
- `glob` 参数在单文件模式下保持 basename post-filter 生效(两个引擎一致,零特判)。
- 工具 description 更新:说明 `path` 可为文件或目录。
- 修复后 spill hint 原文(指向 `xizhi_read_file`/`xizhi_grep`)无需改动即成立。

不改动:`xizhi_list_files`/`xizhi_tree`/`xizhi_glob_files` 的同类目录限制(保持仅目录语义);`path` 必填、`.` 表根、`ValidatePath` 安全校验、分页与输出模式等既有行为。

## Capabilities

### New Capabilities

(无)

### Modified Capabilities

- `xizhi-grep-files`: `path` 参数语义从"搜索起始目录(仅目录)"扩展为"目录或普通文件";新增单文件搜索模式的行为要求(match 的 `file` 字段、glob 过滤、binary 跳过、目录模式行为不变)。

## Impact

- 代码:`internal/tool/xizhi/grep_engine.go`(stat 分支)、`internal/tool/xizhi/grep_rg.go`(cmd.Dir 与搜索目标)、`internal/tool/xizhi/grep.go`(Go 引擎 hidden 豁免)、`internal/tool/xizhi/register.go`(description)。
- 测试:`internal/tool/xizhi/` 现有 grep 测试补充单文件路径用例(含 hidden 根文件、glob 过滤、二进制文件、rg 与 Go 双引擎一致性)。
- 无 config / DB / HTTP API / 前端变更;`mcp-call-result-spill` change 不动(其 hint 依赖本修复成立)。
