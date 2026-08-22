## Context

`xizhi_glob_files`(`internal/tool/xizhi/glob.go`)现状:doublestar glob 匹配相对路径、`matches[]` 纯字符串、无分页、无类型信息。仓库已有成熟的双搜索引擎先例:`xizhi_grep` 的 `rgGrepEngine`(shell out 系统 rg,`--json` 输出)+ `goGrepEngine`(纯 Go WalkDir)+ 共享 mapper(`buildGrepResult` 归一化排序/分页/truncated)——本设计完全复刻该架构。

fd 10.4.2 实测(2026-08-22,本机)钉死的语义事实:

| 行为 | 实测结果 |
|------|---------|
| pattern 匹配目标 | **默认仅匹配条目名(basename)**;`-p/--full-path` 才匹配完整路径(且是绝对路径) |
| 无匹配 exit code | **0**(rg 是 1);错误才非 0 |
| 输出 | 每行一个相对路径(cwd 相对);**目录带尾 `/`**(类型可从尾斜杠判别) |
| 空 pattern | 匹配一切(配合 `-t d` 可纯列目录) |
| 搜索路径 | fd 10 **拒绝非目录**搜索路径("is not a directory") |
| 大小写 | 默认 smart-case(全小写 pattern → 不区分大小写),`-s/--case-sensitive`/`-i` 显式钉死 |

## Goals / Non-Goals

**Goals:**

- `xizhi_find` 以 fd 语义查找文件和目录:regex 名字匹配、类型可见/可过滤、深度限制、分页。
- fd / Go 双引擎输出逐字段一致(复刻 rg/grep 的 mapper 架构)。
- 删除 `xizhi_glob_files`,config 迁移错误信息明确指向新名。

**Non-Goals:**

- 不做完整路径匹配模式(fd 的 `-p` 匹配**绝对路径**,会把宿主路径泄进模型可见语义;相对路径匹配则与 fd 原生语义分叉、两引擎都需 post-filter——按名字找已覆盖主场景,需要路径结构时用 `path` 收窄 + basename pattern 组合)。
- 不放宽 `path` 为文件(搜索根必须是目录,对齐 fd 10 与旧 glob;按名字定位文件用 pattern)。
- 不实现 fd 的执行/删除等危险能力(`-x`/`-X`),不做 smart-case(显式 `ignore_case` 参数,可预测性优先)。
- 不动 `xizhi_grep`(文件路径支持见独立 change `xizhi-grep-file-path`)、`xizhi_tree`、`xizhi_list_files`。

## Decisions

### D1:pattern 匹配条目名(basename),RE2 正则,空 = 匹配一切

fd 默认语义(实测确认),同时与 `xizhi_grep` 的 `glob` 参数(basename 匹配)心智一致。空 pattern 合法且匹配一切——`type: "directory"` + 空 pattern 即"列出某目录下所有子目录",fd 的常用形态。旧工具的 glob 语法(`*.go`)在 regex 下写 `\.go$`(description 给示例)。

### D2:双引擎 = 系统 fd shell out + 纯 Go fallback,共享 mapper 归一

- `defaultEngine = sync.OnceValue(exec.LookPath("fd"))`,随后尝试 `fdfind`(Debian/Ubuntu 的 fd-find 包名);均缺失 → Go 引擎。仿 `defaultEngine` 先例,进程内解析一次。
- fd flags 固定化:`--no-ignore`(恒开,对齐 rg 引擎约定与 Go 引擎能力)、`--hidden`(仅 include_hidden)、`-s --case-sensitive`(默认,**显式关掉 smart-case**)/`-i`(ignore_case 时)、`-t f`/`-t d`(type 过滤)、`-d N`(max_depth)、`cmd.Dir = absPath`、pattern 位置参数 + `.`。解析输出:每行一个路径,尾 `/` 判 directory。
- fd 无 `--json`:按行解析即整个解析器;**无匹配 exit 0、错误非 0**(比 rg 简单,不需要 rg 的 exit-1 特判;非 0 即真错误,取 stderr)。
- Go 引擎:`filepath.WalkDir` + `regexp.MatchString`(basename)+ 类型过滤 + 深度过滤 + hidden 过滤,对齐 `goGrepEngine` 的跳过语义(不跟 symlink、不可读条目跳过不中断)。
- 共享 mapper:entries 按路径字典序排序(fd 并行遍历无序,Go WalkDir 逐目录序——排序保证两引擎一致)、`total`/`truncated`/`applied_limit`/`applied_offset` 分页窗口、`maxFindCollect`(10000,仿 `maxGrepCollect`)收集上限置 truncated。

### D3:参数与返回形状

```
xizhi_find(path?, pattern?, type?, max_depth?, ignore_case?, include_hidden?, head_limit?, offset?)
  → {path, pattern, type, total, truncated, applied_limit, applied_offset,
     entries: [{path, type: "file"|"directory"}]}
```

- `path` 可选,默认工作空间根(对齐旧 glob 的空路径回退,**不**沿用 grep 的 path-必填——本工具是 glob 的直接替换,回退语义保持以减少迁移摩擦);必须为目录,否则报错(对齐 fd 10)。
- `pattern` 可选,默认匹配一切。
- `type` 枚举 `file`/`directory`/`any`,默认 `any`(用户核心诉求:文件和目录都能找、都能区分)。
- `max_depth` 正整数,缺省不限;非法值(0/负数)报错。
- `head_limit`/`offset` 默认 200/0,分页语义对齐 `xizhi_grep`(truncated 时 `offset = offset + head_limit` 翻页)。
- entry 的 `path` 相对 `path` 参数(搜索根),与 fd 行输出一致。

### D4:config 迁移——旧 key 加载期拒绝

- `XizhiConfig.GlobFiles`(yaml `glob_files`)→ `Find`(yaml `find`),同为 `XizhiToolConfig`(enabled 开关语义不变)。
- yaml.v3 对未知字段默认静默忽略,故在 config 校验层显式探测原始 `tools.xizhi.glob_files` key(仿仓库对已移除 key 的一贯处理:`agents.<name>.model` 等):存在即 fail-fast,错误信息 `tools.xizhi.glob_files was removed; use tools.xizhi.find (tool xizhi_find)`。
- `agents.<name>.tools` 列表里的 `xizhi_glob_files` 无需专门校验:`Registry.ToolsFor` 对未知名报 `unknown tools [...]`,启动 fail-fast 自带迁移信号;`config.example.yaml` 两处工具列表 + timeouts 示例同步改名。

### D5:周边接线换名(机械同步)

- `internal/agent/orchestrator.go` `isXizhiTool`:`NameGlobFiles` → `NameFind`。
- `internal/tool/executor/register.go` bash steering 文案:`find`→`xizhi_glob_files` 改为 `find`→`xizhi_find`。
- `xizhi_grep` description 末句 "Use `glob` to filter…" 指 grep 自身参数,不动;核对无其他 `glob_files` 文案引用。
- CLAUDE.md 工具族清单、config.example.yaml 注释同步。

## Risks / Trade-offs

- [fd 与 Go 引擎语义漂移(fd 版本差异:smart-case 默认、尾斜杠约定、hidden 语义)] → flags 固定化只用长期稳定的基础 flag(`-s`/`-i`/`-t`/`-d`/`--hidden`/`--no-ignore`);集成测试断言两引擎同参数输出逐字段一致(仿 grep 的双引擎测试)。
- [尾 `/` 判目录在 `--path-separator` 差异平台出问题] → 仅支持 POSIX 路径(工作空间工具全部是 `/` 语义);Go 引擎自行判 `d.IsDir()`,不依赖解析。
- [regex 误用旧 glob 习惯(`*.go` 报 RE2 编译错误 `missing argument to repetition`)] → 已知破坏性,description 首句给 `\.go$` 示例并显式说明 pattern 是 regex;错误信息可读。
- [match-all(空 pattern)在全工作空间跑满 10000 收集上限] → 分页默认 200 + collect cap + truncated 标记;description 引导用 `path`/`type`/`max_depth` 收窄。
- [运维没装 fd 且不知道] → 无功能差异(仅性能),不告警;文档说明可选依赖(与 rg 同等待遇)。

## Migration Plan

1. 部署:更新 config.yaml 三处(`tools.xizhi.glob_files` → `tools.xizhi.find`、`agents.*.tools` 列表、`tools.timeouts` key),随新二进制一起上线。
2. 遗留 `glob_files` key:启动即拒(错误指向迁移);遗留 `agents.*.tools` 旧名:启动 `unknown tools` fail-fast。
3. 回滚:还原 config.yaml 三处 + 旧二进制;无持久化痕迹(工具无状态)。
