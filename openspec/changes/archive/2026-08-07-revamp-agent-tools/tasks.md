## 1. Config 基础

- [x] 1.1 `internal/config/config.go`：`XizhiConfig` 增加 `Grep XizhiToolConfig \`yaml:"grep"\`` 字段（紧随 `GlobFiles` 之后），并在 `XizhiConfig` 相关默认/校验处确认 `grep` 默认跟随现有 enabled 模式（不强制默认 true，由 example 引导；若存在 `applyDefaults` 则对齐其它 xizhi 子工具）。
- [x] 1.2 `internal/config/config.go`：删除 `PipToolConfig` 类型及其全部方法（`NetworkEnabled`/`ToExecutorToolConfig`/`ApplyDefaults`/`DefaultPipToolConfig`）；从 `ExecutorConfig` 删除 `Python ExecutorToolConfig` 与 `Pip PipToolConfig` 字段。
- [x] 1.3 `internal/config/config.go`：`DefaultExecutorToolConfig()` 的 `Network` 默认值由 `false` 改为 `true`（使 bash 默认放开网络，对齐旧 pip_install 默认姿态）。
- [x] 1.4 `config.example.yaml`：删除 `tools.executor.python` 与 `tools.executor.pip` 段（含其注释）；把 `tools.executor.bash.network` 注释改为「默认 true，使 pip-via-bash 可达 PyPI；可设 false 收紧」；在 `tools.xizhi` 段新增 `grep: { enabled: true }` 示例（紧随 `glob_files` 之后），并加注释说明 `xizhi_grep` 需在 agent tools 列表中列出方可使用。

## 2. xizhi_grep 工作区内容搜索工具

- [x] 2.1 新建 `internal/tool/xizhi/grep.go`：实现 `GrepFiles(workspaceRoot, path, pattern, glob, ignoreCase, includeHidden, contextBefore, contextAfter)` —— 用 `filepath.WalkDir` 遍历 `path`、按 `glob`(doublestar) 过滤文件名、`bufio.Scanner` 逐行读、`regexp`(RE2) 匹配；跳过含 NUL 字节的二进制文件；不跟随符号链接（对齐 `GlobFiles` 用 `doublestar.WithNoFollow` 的精神，遍历时跳过 symlink 目录）；返回 `{path, pattern, glob, ignore_case, matches:[{file, line_number, line, context_before?, context_after?}], truncated}`。
- [x] 2.2 `grep.go` 内置结果上限：匹配总数上限（约 200）、每行截断（约 500 字符），超限停止追加并置 `truncated: true`；`path` 经 `validatePath`（拒 `.blowball`、绝对路径、`..`、symlink 逃逸）；正则编译失败返回错误；`ignore_case` 为 true 时用 `(?i)` 或 `regexp.IgnoreCase`。
- [x] 2.3 `internal/tool/xizhi/register.go`：新增常量 `NameGrep = "xizhi_grep"`、`schemaGrep`（含 path/pattern/glob/ignore_case/include_hidden/context_before/context_after）、`grepArgs` 结构；在 `RegisterAll` 中按 `cfg.Grep.Enabled` 条件注册 `xizhi_grep`（模式与 glob_files/delete 一致）。
- [x] 2.4 新建 `internal/tool/xizhi/grep_test.go`：覆盖正则匹配、glob 过滤、ignore_case、context_before/after、二进制跳过、`.blowball` 拒绝、结果上限截断、非法正则报错、从根默认搜索、隐藏文件默认排除。

## 3. luban_read_skill 放开扩展名 + 二进制拒绝

- [x] 3.1 `internal/tool/skill/skill.go`：`validateSkillSubPath` 删除「非 `.md` 拒绝」的扩展名校验（`if ext != ".md"` 分支）；其余绝对路径/`..`/symlink 安全校验不变。
- [x] 3.2 `internal/tool/skill/skill.go`：`ReadPath` 在读取后、返回前做二进制检测（内容含 NUL 字节 → 返回明确二进制拒绝错误，对齐 workspace `WriteContent` 的 `BINARY_FILE` 措辞）；frontmatter 剥离保持现状（`parseFrontmatter` 仅 `---` 开头才剥，对非 md 自动 no-op）。
- [x] 3.3 `internal/tool/luban/register.go`：更新 `luban_read_skill` 工具描述，把「only `.md` files」改为「任意文本文件（二进制被拒绝）」，`path` 参数描述同步去掉 `.md` 限制并注明二进制会被拒。
- [x] 3.4 `internal/tool/luban/luban_test.go`：新增/改写用例——读取非 md 文本资产（如 `.py`/`.yaml`）成功；读取二进制文件（含 NUL 字节）被拒；原有 `.md` 子文档读取与安全校验（绝对路径/`..`/symlink/超限/not-found）用例保持通过。

## 4. executor 精简为单一 bash

- [x] 4.1 `internal/tool/executor/register.go`：删除 `registerPython`、`registerPip`、`pythonArgs`、`pipArgs`、`buildPipCommand`、`resolvePythonFile` 函数。
- [x] 4.2 `internal/tool/executor/schemas.go`：删除 `schemaPython`、`schemaPip`（保留 `schemaBash`）。
- [x] 4.3 `internal/tool/executor/executor.go`：删除常量 `ToolPython`、`ToolPip`；`RegisterAll` 仅保留 bash 分支（删 python/pip 的 `if cfg.Enabled` 分支）。
- [x] 4.4 `internal/tool/executor/runner.go`：确认 `pipTargetPath`/PYTHONPATH 注入与 `logDangerousCommand`/audit 仍对 bash 生效（保留不变，作为 pip-via-bash 桥梁）。
- [x] 4.5 `cmd/blowball/serve.go`：`executorConfigured()` 简化为 `return cfg.Tools.Executor.Bash.Enabled`；更新相关注释（323/440/569 行附近）把「bash/python/pip」改为「bash」。
- [x] 4.6 `internal/tool/executor/` 测试清理：`executor_test.go`/`register_test.go`/`bwrap_test.go`/`runner_test.go`/`env_test.go` 中删除/改写所有针对 `python`/`pip_install` 的用例，仅保留 bash 相关；确保 bash 的 PYTHONPATH 注入、网络、tmp、$HOME、PATH 用例继续通过。

## 5. bash 工具描述强化（纯提示词，零代码拦截）

- [x] 5.1 `internal/tool/executor/register.go`：更新 `registerBash` 的 `Description`，把让位反模式名单扩充为 `cat`/`rm`/`ls`/`find`/`sed`/`awk`/`grep`，并逐个指向专用工具（`cat`→`xizhi_read_file`；`ls`→`xizhi_list_files`/`xizhi_tree`；`find`→`xizhi_glob_files`；`grep`→`xizhi_grep`；`sed`/`awk`→`xizhi_modify_file`；`rm`→`xizhi_delete`），保留 `DO NOT` 强指令词与 64KB 截断等既有约束。
- [x] 5.2 确认未引入任何针对这些关键词的命令入参拦截/改写代码（仅描述引导；`dangerousCommandPattern` warn audit 保持不动）。

## 6. prompt 渲染与下游 staleness

- [x] 6.1 `internal/prompt/render.go`（约 135 行）：Skills 段落引导由「use the bash or python tools」改为「use the bash tool」（运行技能内 Python 脚本经 `bash` 调 `python3`）。
- [x] 6.2 `internal/prompt/render.go`（约 155 行）：执行器工作目录说明由「The `bash` and `python` sandboxes」改为仅「The `bash` sandbox」。
- [x] 6.3 更新 `internal/prompt` 相关测试（若有断言该文案的用例）使其与新文案一致。

## 7. 文档同步

- [x] 7.1 `CLAUDE.md`：更新「Executor tools (Linux only)」段落（删 python/pip_install 说明、bash 网络默认放开、新增 `xizhi_grep` 与 luban 读任意文本文件的说明）；更新「Built-in tool families」里 `executor/`（仅 bash）与 `xizhi/`（加 `xizhi_grep`）与 `luban/`（read_skill 放开）的描述；更新 `needsLubanTools` 附近与工具列表的措辞。
- [x] 7.2 检查 `api/openapi.yaml` 是否提及 python/pip 工具或 xizhi 工具清单，按需同步（grep 工具一般不进 OpenAPI，确认即可）。

## 8. 验证

- [x] 8.1 `make build` 通过（确认删除 PipToolConfig/python/pip 后无编译残留引用）。
- [x] 8.2 `make test`（含 `go test ./internal/tool/...`、`./internal/config/...`、`./test/integration/...`）全部通过。
- [x] 8.3 `make lint` 通过。
