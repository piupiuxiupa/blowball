# Implementation Tasks

> 所有任务均为**纯文案 / 收敛 / 去重**，零运行时行为变更。优化后描述文案以 `design.md` D1/D5 为准（参数级 `description` 见 D5）。每个工具改完即应满足对应 delta spec 的 Scenario。

## 1. xizhi 工具描述（结果结构 + 让位反模式）

- [x] 1.1 `internal/tool/xizhi/register.go` `NameReadFile`：`Description` 改为 design D1 的 `xizhi_read_file` 文案（声明返回 `{path, content, size}`、全文无行号无截断、缺失文件报错、优先于 bash/python 的 `cat`）。
- [x] 1.2 `internal/tool/xizhi/register.go` `NameWriteFile`：`Description` 改为 D1 文案（返回 `{path, size, absolute}`、自动建父目录、覆盖既有文件、优先于 `echo`/重定向）。
- [x] 1.3 `internal/tool/xizhi/register.go` `NameModifyFile`：`Description` 改为 D1 文案（返回 `{path, old_size, new_size}`、`old_content` 唯一匹配否则失败）。
- [x] 1.4 `internal/tool/xizhi/register.go` `NameListFiles`/`NameTree`/`NameGlobFiles`：`Description` 补结果结构（list `{path, entries[]}`、tree `{path, depth, tree[]}`、glob `{path, pattern, matches[]}`），`Return` → `Returns`，保留 depth 默认 3/上限 10、hidden 默认 false。
- [x] 1.5 `internal/tool/xizhi/register.go`：参数级 `description` 微调（`path` 明确"相对工作区根"；`include_hidden` 内联 "Defaults to false"；`depth` 内联默认 3/上限 10；`pattern` 保留 doublestar 示例）——仅润色，不改 schema 结构。

## 2. webfetch 工具描述与参数

- [x] 2.1 `internal/tool/webfetch/register.go`：`Description` 按 D1 风格润色（动词开头、明确返回 final url/status/headers/body），**完整保留**既有"description guides error recovery"要求所规定的错误恢复指引文案（非 2xx/超限时结果带最终状态码与 headers 含 `Location`，可据 Location/解析 URL/调整 method/headers 重试）——见 `openspec/specs/webfetch/spec.md` 既有要求，不得删减。
- [x] 2.2 `internal/tool/webfetch/register.go` `schemaFetch`：`url` 描述由 `"URL to fetch."` 改为 `"Absolute http(s) URL to fetch (must include the scheme)."`；`method`/`headers` 描述按 D5 内联默认/语义（method 默认 GET）。

## 3. executor 工具描述（结果结构 + 上限/超时 + 反模式 + 互斥）

- [x] 3.1 `internal/tool/executor/register.go` `registerBash`：`Description` 改为 D1 文案（返回 `{output, exit_code, truncated}`、`output` 为合并 stdout+stderr、64KB 上限 + `...output truncated...` 标记 + `truncated:true`、默认 30s 超时、沙箱无网络只读约束），并加反模式 bullet（文件读写/搜索优先 `xizhi_*`，避免 `cat`/`echo`/重定向/`find`/`grep`）。
- [x] 3.2 `internal/tool/executor/register.go` `registerPython`：`Description` 改为 D1 文案（同 bash 的结果结构与上限/超时；声明 `code` 与 `file` 互斥、恰提供一个；`/workspace/.pip` 经 PYTHONPATH 可用），并加反模式 bullet（文件读写优先 `xizhi_*`）。
- [x] 3.3 `internal/tool/executor/register.go` `registerPip`：`Description` 改为 D1 文案（返回 pip 的 `{output, exit_code, truncated}`、默认 120s 超时、网络默认开；用于装依赖解决 `ModuleNotFoundError`/`ImportError`，勿用于跑代码）。
- [x] 3.4 `internal/tool/executor/schemas.go`：参数级 `description` 按 D5 润色——`command`（沙箱内 shell 命令）、`code`（`python3 -c` 内联代码，与 `file` 互斥）、`file`（工作区内相对路径；绝对路径仅用于只读 skill 目录脚本）、`packages`（至少一个、可带版本约束，保留示例）、`upgrade`（true 时传 `--upgrade`）。不改 schema 结构（`oneOf` 保留）。

## 4. luban 描述（收敛 cross-rule + markdown body 契约）

- [x] 4.1 `internal/tool/luban/register.go` `registerReadSkill`：`Description` 改为 D1 文案（返回 `SKILL.md` markdown body、已剥离 frontmatter、用户优先全局、`name` 为简单标识非路径），保留一条最简指针"用 luban 而非 `xizhi_*` 访问 skill 目录"。
- [x] 4.2 `internal/tool/luban/register.go` `registerListSkills`：`Description` 去掉 "Never use xizhi_* tools to access the skills directory." 整句，保留"返回合并后的 skill 元数据列表（用户覆盖全局）、用 list 发现再 read"的职责。
- [x] 4.3 `internal/tool/luban/register.go` `registerInstallSkill`：`Description` 保留四种安装形态 + 单文件下载错误恢复指引（既有 spec 要求），去掉重复的 "Never use xizhi_* ..." 整句。

## 5. invoke_* 描述去重（单一来源）

- [x] 5.1 `internal/agent/tools.go`：导出 `invoke_chongzhi`/`invoke_liang` 的描述为单一来源（如 `InvokeChongzhiDescription`/`InvokeLiangDescription`，或 `InvokeDescription(name string) string`）；`buildConfuciusToolsJSON` 改为引用该来源。措辞按 D1 润色（动词开头、点明用途），语义不变。
- [x] 5.2 `internal/handler/mcp.go`：`invokeDescription` 改为复用 `agent` 包导出的单一来源，删除硬编码副本，使 MCP 目录与模型 `tools[]` 不再可能漂移。

## 6. 系统 prompt 权威源

- [x] 6.1 `internal/prompt/render.go`：确认 Skills 段落（约 `:99-103`）保留权威表述 "Use luban_list_skills / luban_read_skill / luban_install_skill for skill operations. Never use xizhi_* tools to access the skills directory."（与 delta spec `luban-skill-tools` MODIFIED 的 Scenario 一致）；若措辞与该 Scenario 不完全一致，对齐之。不新增协作规则。

## 7. 测试（描述契约断言，沿用 webfetch 范式）

- [x] 7.1 `internal/tool/xizhi/register_test.go`（或同包测试）：新增断言——read/write/modify 的 `Description` 分别含 `content`/`size`、`absolute`、`old_size`/`new_size`，且 modify 描述含"唯一匹配"语义短语（如 `exactly one` / `unique`）。
- [x] 7.2 `internal/tool/executor/register_test.go`：新增断言——bash/python/pip 的 `Description` 均含 `output`、`exit_code`、`truncated`、`64KB`（或 `truncat`）；bash/python 描述含 `xizhi` 让位短语；python 描述含 `code`/`file` 互斥语义。
- [x] 7.3 `internal/tool/webfetch/` 测试：新增/保留断言——`Description` 含错误恢复指引关键短语（`Location`、retry/重试语义）且 `url` 参数描述含 `http`；确保既有 "description guides error recovery" 要求仍满足。
- [x] 7.4 `internal/tool/luban/luban_test.go`：新增断言——`luban_read_skill` 描述含 markdown body 语义且保留最简 `xizhi_*` 指针；`luban_list_skills`/`luban_install_skill` 描述**不再**逐字含 "Never use xizhi_* tools to access the skills directory."。
- [x] 7.5 `internal/agent/` 与 `internal/handler/mcp_test.go`：新增断言——`invoke_chongzhi`/`invoke_liang` 在 `tools.go` 与 `mcp.go` 取自同一来源（如断言 `handler` 暴露的描述 == `agent.InvokeDescription(...)`，杜绝双写）。

## 8. 验证

- [x] 8.1 `openspec validate optimize-builtin-tool-descriptions` 通过；delta spec 的每个 Scenario 在实现后均可对应到一条断言或人工核验。
- [x] 8.2 `go build ./...` 通过。
- [x] 8.3 `go test -race ./internal/tool/... ./internal/agent/... ./internal/handler/... ./internal/prompt/...` 通过。
- [x] 8.4 `make lint`（`go vet ./...`）通过；gofmt 已对所有改动文件 `-w` 归整。
- [x] 8.5 人工抽查：构造一次 agent 请求，确认渲染出的 `tools[]` 里执行类工具描述含 `{output,exit_code,truncated}` 与 64KB/超时，且 luban 描述无重复 cross-rule 整句、系统提示词保留权威表述。
