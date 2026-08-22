## Context

`xizhi_grep` 的两个搜索引擎都以目录为根:

- **rg 引擎**(`internal/tool/xizhi/grep_rg.go`):`exec.CommandContext(ctx, rgPath, args...)` 且 `cmd.Dir = in.absPath`,搜索目标硬编码为 `"."` —— cwd 必须是目录。
- **Go 引擎**(`internal/tool/xizhi/grep.go`):`filepath.WalkDir(in.absPath, ...)`,对单文件路径天然可行(fn 只被调用一次、`d.IsDir()` 为 false 直接进入扫描分支),但被 `grepRun` 的前置检查挡住:`os.Stat` 后 `!info.IsDir()` 即报 `xizhi grep: %q is not a directory`。

触发场景:mcp-call-result-spill 把超大 MCP 结果落盘为单文件,hint 引导模型用 `xizhi_grep` 读回,模型传文件路径即报错。mcp-call-result-spill 的设计假设"落盘文件对 xizhi_grep 直接可读"在 grep 一侧不成立。

## Goals / Non-Goals

**Goals:**

- `xizhi_grep` 的 `path` 接受普通文件:仅搜索该文件,结果形状与目录模式完全一致。
- rg 与 Go 双引擎在单文件模式下输出逐字段一致(与目录模式的 parity 约定相同)。
- 零 config / API / 持久化变更;目录模式行为逐字节不变。

**Non-Goals:**

- 不放宽 `xizhi_list_files` / `xizhi_tree` / `xizhi_glob_files` 的同类目录限制(它们语义上就是目录列举工具)。
- 不改动 `path` 必填、`.` 表根、`ValidatePath` 安全校验、输出模式与分页等既有行为。
- 不改 mcp-call-result-spill(修复后其 hint 原文自然成立)。

## Decisions

### D1:stat 分支放宽为"目录或普通文件"

`grepRun` 中 `os.Stat` 后:目录照旧;`info.Mode().IsRegular()` 为 true 进入单文件模式;其余(符号链接经 `ValidatePath` 已解析为真实路径、fifo/socket 等非常规条目)保持原 `is not a directory` 错误,错误文案改为 `"must be a file or directory"` 以反映新语义。

替代方案:报错文案不动、仅在内部区分——否决,旧文案"is not a directory"在新语义下是假话。

### D2:`grepInput` 增加单文件标记,引擎按需分流

`grepInput` 增加 `searchFile string`(空 = 目录模式;非空 = 要搜索的单个文件 basename,同时 `absPath` 仍指向该文件的绝对路径)。

- **rg 引擎**:`cmd.Dir = filepath.Dir(in.absPath)`,args 的搜索目标从 `"."` 改为 `in.searchFile`(目录模式传 `"."`,单文件模式传文件名)。`--json` 输出的 `data.path.text` 即为所传的文件名,`TrimPrefix("./")` 后与目录模式的相对路径形状统一,无需新映射。
- **Go 引擎:不分流**。`filepath.WalkDir` 对单文件路径天然只回调一次,现有扫描分支直接工作;唯一适配是 hidden 过滤豁免——当前文件级过滤(`!in.includeHidden && isHiddenName(d.Name())` 即跳过)会把以 `.` 开头的根文件自身跳过,单文件模式下 `p == in.absPath` 时跳过该检查(目录模式对根目录已有同样的豁免先例)。

替代方案:Go 引擎也显式分流、绕过 WalkDir 直接 `scanGrepFileRaw`——否决,WalkDir 单文件行为正确且免维护一个分叉路径;显式分流反而引入两份"遍历入口"语义。

### D3:glob 过滤保持 basename 生效

单文件模式下 `glob` 参数仍按文件 basename post-filter(两个引擎的 glob 过滤本来就都是收集后的 `doublestar.Match` post-filter,天然一致)。理由:`glob` 与 `path` 矛盾时(如 `path: a/b.txt`,`glob: "*.go"`)结果为空是可预期行为;忽略 glob 则引入"单文件模式 glob 无效"的特判语义。description 不需要为此加说明(basename 语义本就是现状描述)。

### D4:match 的 `file` 字段为 basename

单文件模式下 match 的 `file` 为文件 basename(相对 `path` 的路径,`path` 即文件时相对路径 = basename)。这是 D2 的自然结果,与 `filepath.Rel(absPath, p)` 在单文件时的取值一致,无需任何特殊处理。`result.Path` 照旧返回传入的 `relPath`(文件路径)。

### D5:description 一句话更新

`register.go` 中 `xizhi_grep` description 的 `path` 说明补充 "may be a file (search just that file) or a directory"。其余 steering 文案不动。

## Risks / Trade-offs

- [rg 引擎在 cmd.Dir 非工作目录下解析相对路径] → 搜索目标是 basename、cwd 是其父目录,rg 不涉及其他相对引用;已由 `--json` 输出路径等于所传目标保证。
- [隐藏根文件行为分歧(rg `--hidden` 未传时同样跳过隐藏文件)] → 两引擎一致:单文件模式下根文件自身的 hidden 跳过均豁免(`include_hidden` 语义只作用于遍历中遇到的条目);Go 侧显式豁免,rg 侧对显式给出的文件目标不做 hidden 过滤(rg 对显式路径参数不过滤,仅对遍历发现的条目应用 `--hidden` 规则),需测试确认并在 Go 侧对齐。
- [旧测试断言 `is not a directory` 文案] → 仅本包内单测可能引用,随 D1 一并更新。

## Migration Plan

纯代码变更,无 schema/config/API 兼容面;部署即生效,回滚即还原二进制。
