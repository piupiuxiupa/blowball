## Context

工具描述是模型在每次请求里唯一能读到的"工具说明书"（`name + description + input_schema`），它决定**何时调 / 怎么填参 / 返回值怎么用**。当前内置工具描述由各 `register.go` 里硬编码的 `Description` 字符串 + JSON Schema 内的参数级 `description` 组成，经 `tool.Registry.OpenAITools`（`internal/tool/registry.go:145`）渲染进 `tools[]`，同时经 `internal/handler/mcp.go` 的 `Tools` 暴露给 `/api/v1/mcp/tools` 目录。

逐条对照"工具描述优化规则集"（5 层骨架 + R1–R10），现有描述的主要缺陷：

| 规则 | 缺陷 | 受影响工具 |
|------|------|-----------|
| R2 输出契约 | 多数工具不说返回什么、是否截断 | `xizhi_read_file/write_file/modify_file`、`bash`、`python`、`pip_install`、`luban_read_skill` |
| R3 反模式 | 通用执行工具不"让位"给专用文件工具 | `bash`、`python`、`pip_install` |
| R5/R6 默认/单位/自解释 | 裸参数、缺默认值/单位 | `webfetch.url`、执行类工具的 `command/code/file` |
| R8 参数依赖 | 互斥参数未在描述声明 | `python`（`code` 与 `file` 互斥） |
| R10 全局规则勿入工具描述 | 跨工具协作规则在 3 个 luban 描述 + 系统提示词重复 | `luban_list_skills/luban_read_skill/luban_install_skill` |
| H1 双写 | 同一描述两处硬编码，易漂移 | `invoke_chongzhi/invoke_liang`（`agent/tools.go` vs `handler/mcp.go:invokeDescription`） |

运行时事实（决定输出契约的措辞，已核对源码）：

- 执行类工具结果 `ExecutionResult{Output, ExitCode, Truncated}`（`internal/tool/executor/runner.go:29`）；`Output` 是 stdout+stderr 合并；默认 `MaxOutputBytes=65536`（64KB）、超限追加 `...output truncated...` 并置 `Truncated=true`（`runner.go:21,154`）；默认 `Timeout`：bash/python 30s、pip 120s（`config.go:554,579`）。
- `xizhi` 结果：read `{path,content,size}`（全文、无行号、无截断，`read.go:10`）；write `{path,size,absolute}`（`write.go:10`）；modify `{path,old_size,new_size}`（`modify.go:11`）；list `{path,entries[]}`；tree `{path,depth,tree[]}`；glob `{path,pattern,matches[]}`。
- 系统 prompt 已含权威的"不要用 `xizhi_*` 访问 skill 目录"表述（`internal/prompt/render.go:102`）。

约束：**零运行时行为变更**——不动参数 schema、不动返回结构、不动 API 契约、不动 DB/SSE/角色；只改文案 + 两处收敛/去重重构。MCP 远端工具描述由远端服务器提供，不在范围。

## Goals / Non-Goals

**Goals:**

- 为所有自研内置工具补齐**输出契约**（R2），尤其是执行类工具的 `{output,exit_code,truncated}` + 64KB 截断 + 超时。
- 为 `bash`/`python`/`pip_install` 补齐**反模式**（R3），把文件读写/搜索让位给 `xizhi_*`。
- 修掉裸参数与未声明的参数依赖（`webfetch.url`、`python` 的 `code/file` 互斥）（R5/R6/R8）。
- 把"不要用 `xizhi_*` 访问 skill 目录"收敛为系统提示词单一权威源，luban 描述只留最简指针（R10）。
- 消除 `invoke_*` 描述双写（H1），改为 `agent` 包导出单一来源、`mcp.go` 复用。
- 用"描述包含关键契约短语"的断言把上述契约固化（沿用 `webfetch` 既有测试范式）。

**Non-Goals:**

- 不改任何工具的运行时行为、参数 schema、返回值结构。
- 不改 API 契约（`api/openapi.yaml`）、DB 迁移、SSE/持久化、角色划分。
- 不优化 MCP 远端工具的描述（由远端提供）。
- 不引入 H2（按当前工具集动态生成反模式）——当前 `bash`/`python` 只挂在 Chongzhi（必带 `xizhi_*`）上，静态引用 `xizhi_*` 总是成立的，无需动态化。若未来出现"有 bash 但无 xizhi"的 agent 配置，再启动 H2。
- 不引入 H3（运行时事后注入提示）——超出本次"描述文案"范围。

## Decisions

### D1：为每个工具补齐输出契约（R2），措辞与真实返回结构对齐

逐工具给出**优化后顶层 description**（参数级 `description` 见 D5）。动词开头（R1 已基本满足，个别 `Return` → `Returns`）。**每个工具不少于 2 处强指令词（R4）**——对其致命约束用**加粗 + 大写**关键词（`MUST`/`DO NOT`/`NEVER`/`IMPORTANT`/`REQUIRED`/`SHOULD`/`ONLY`/`MUST NOT` 等）标记；大写保证即使 markdown 不渲染仍可见。

**xizhi_read_file**
> Reads a workspace file and returns `{path, content, size}` with the full contents as a UTF-8 string — no line-number prefix, no truncation. **`path` MUST be relative to the workspace root** (absolute paths, `..` and symlink escapes are rejected); a missing file returns an error. **DO NOT read files with `bash`/`python` (`cat`) — use this tool.**

**xizhi_write_file**
> Creates or overwrites a workspace file with the given text content and returns `{path, size, absolute}`. Parent directories are created automatically. **IMPORTANT: an existing file at `path` is overwritten.** **DO NOT write files with `bash`/`python` (`echo`/redirects) — use this tool.**

**xizhi_modify_file**
> Performs an exact string replacement in a workspace file and returns `{path, old_size, new_size}`. **`old_content` MUST match exactly one location** — the call fails if the match is missing or appears more than once. **DO NOT rewrite the whole file** — use this for targeted edits and `xizhi_write_file` for full rewrites.

**xizhi_list_files**
> Lists the immediate children of a workspace directory and returns `{path, entries[]}` where each entry carries `name`, `type` (`file`/`directory`) and `size`. **`path` MUST be relative to the workspace root.** **This lists one level only — DO NOT expect recursion; use `xizhi_tree` for nested listings.** Hidden entries (names starting with `.`) are excluded unless `include_hidden` is true.

**xizhi_tree**
> Returns `{path, depth, tree[]}`, a nested representation of a workspace directory. **`path` MUST be relative to the workspace root.** **`depth` defaults to 3 and MUST NOT exceed 10.** Hidden entries are excluded unless `include_hidden` is true.

**xizhi_glob_files**
> Searches a workspace directory with a doublestar glob pattern and returns `{path, pattern, matches[]}` of relative paths. **`path` MUST be relative to the workspace root.** **`pattern` MUST be a doublestar pattern** (e.g. `src/**/*.go`, `**/*_test.go`); hidden entries are excluded unless `include_hidden` is true.

**bash**
> Executes a shell command inside a sandboxed workspace and returns `{output, exit_code, truncated}`: `output` is combined stdout+stderr, `exit_code` is the process exit status. **IMPORTANT: output is capped at 64KB** — when truncated it ends with `...output truncated...` and sets `truncated: true`; if you need the rest, narrow the command or redirect output to a workspace file and read it with `xizhi_read_file`. Commands time out at 30s by default. The sandbox runs as an unprivileged user with no network by default; only `/workspace` is writable. Global skills are read-only at `/skills/global`; per-user skills live at `/workspace/.blowball/skills` (managed via luban).
> - **DO NOT use `cat`, `echo`/redirects, `find` or `grep` for file work — use the `xizhi_*` tools** unless a dedicated tool cannot do the job.

**python**
> Executes Python code or a Python file inside a sandboxed workspace and returns `{output, exit_code, truncated}` (same shape, 64KB cap and 30s timeout as `bash`). **Exactly one of `code` or `file` is REQUIRED (mutually exclusive).** Packages installed under `/workspace/.pip` are available automatically via `PYTHONPATH`. The sandbox runs as an unprivileged user with no network by default; only `/workspace` is writable.
> - **DO NOT do file I/O from Python — use the `xizhi_*` tools** for reading/writing workspace files unless the task needs computation.

**pip_install**
> Installs Python packages into the workspace via pip (inside the sandbox) so they are available to the `python` tool via `PYTHONPATH`. **Use this ONLY to install dependencies — when `python` fails with `ModuleNotFoundError`/`ImportError`.** Returns `{output, exit_code, truncated}` (same 64KB output cap as `bash`; 120s timeout by default; network enabled by default). **DO NOT use this to run code — use `python` for that.**

**luban_read_skill**
> Reads a skill by name and returns its `SKILL.md` markdown body (YAML frontmatter stripped). User skills take precedence over global skills. **`name` MUST be a simple skill identifier, not a path.** **DO NOT read skills with `xizhi_*` — use luban.** (Skill-directory access rules live in the system prompt.)

**luban_list_skills / luban_install_skill**：`luban_list_skills` 返回合并后的 skill 元数据列表，强指令词为 **OVERRIDE**（用户覆盖全局）与 **MUST**（先 list 发现、再 `luban_read_skill` 读取）；去掉重复整句 cross-rule。`luban_install_skill` 保留四种安装形态 + 错误恢复指引，强指令词为 **IMPORTANT**（同名覆盖）与 **SHOULD**（按 Location 重试）；去掉重复的 cross-rule 整句。

**invoke_chongzhi / invoke_liang**（去重重构来源见 D4）：保留"Invoke the Chongzhi/Liang sub-agent …"语义并按 R3/R4 强化——各加一条"何时别用我"反模式与 ≥2 处强指令词（chongzhi：**MUST** 需改文件时调 / **DO NOT** 分析类改用 `invoke_liang`；liang：**MUST NOT** 改文件 / **DO NOT** 改文件改用 `invoke_chongzhi`）。

> 说明：luban 工具描述里原有的"Never use `xizhi_*` tools to access the skills directory"**不是删除规则**，而是收敛为最简指针 + 系统提示词权威源（见 D3）。

### D2：执行类工具的反模式让位给 `xizhi_*`（R3）

`bash`/`python` 是"最后一道通用口子"，但当前没有告诉模型"读写文件请用专用工具"。新增一句让位指引（见 D1 文案中的 bullet）。这是最常见误用（`cat`/`echo`/重定向代替 `xizhi_*`）。`pip_install` 的反模式是"别用它跑代码，跑代码用 `python`"。

- **备选（把反模式写进系统提示词而非工具描述）**：规则集 R10 的边界是"某工具的专属用法写工具描述，跨工具协作写系统提示词"。"读写文件用 xizhi 不用 cat"本质是 `bash` 这个工具的专属用法（指向同生态的专用工具），属于工具描述范畴，**采纳写入工具描述**。

### D3：把"不要用 `xizhi_*` 访问 skill 目录"收敛为系统提示词单一权威源（R10）

当前该规则在 3 个 luban 描述里各出现一次整句，且系统提示词（`render.go:102`）已有权威表述。按 R10，跨工具协作规则应有单一权威源。

- **做法**：`render.go` 的 Skills 段落保持为权威表述（已存在，必要时补强措辞）；luban 工具描述里**移除重复整句**，仅在 `luban_read_skill`（最易被误用 `xizhi_read_file` 替代的入口）保留一句最简指针"用 luban，不要用 `xizhi_*` 访问 skill 目录"。`luban_list_skills`/`luban_install_skill` 去掉该整句。
- **安全姿态不变**：模型仍被明确告知不要用 `xizhi_*` 访问 skill 目录——只是权威源收敛到系统提示词，且 `xizhi_*` 的 `validatePath` + Landlock 仍是应用层与内核层防御，不依赖描述文案。
- **既有 spec 要求 `Tool descriptions guide model away from xizhi`（`luban-skill-tools/spec.md:84`）随此收敛 MODIFIED**：从"luban 描述 + 系统提示词 都 SHALL 明确告知"改为"系统提示词为权威源 SHALL 明确告知；`luban_read_skill` 描述 SHALL 保留最简指针"。

### D4：消除 `invoke_*` 描述双写（H1）

`internal/agent/tools.go:25,30` 与 `internal/handler/mcp.go:73-80`（`invokeDescription`）各硬编码一份相同的 `invoke_chongzhi`/`invoke_liang` 描述，改一处会与另一处漂移，导致 MCP 目录与模型实际看到的 `tools[]` 不一致。

- **做法**：在 `agent` 包导出描述（如 `var InvokeChongzhiDescription = ...` 与 `InvokeLiangDescription = ...`，或一个 `InvokeDescription(name) string`）。`tools.go` 的 `buildConfuciusToolsJSON` 与 `mcp.go` 的 `invokeDescription` 都改为复用该单一来源；删除 `mcp.go` 里的硬编码副本。
- **备选（保留两处，加测试防漂移）**：仍有两个真值源，测试只能事后发现漂移。**采纳单一来源**，从结构上消除漂移。
- 语义：dispatch 行为不变；描述文案按 R3/R4 强化（加"何时别用我"反模式与 ≥2 处强指令词），仅措辞层面变化，故 `agent-orchestration` 不新增 spec 要求。

### D5：参数级描述自解释（R6）+ 互斥/依赖（R8）

- `webfetch.url`：`"URL to fetch."` → `"Absolute http(s) URL to fetch (must include the scheme)."`.
- `python.code`/`file`：在描述中显式声明二者互斥（与 schema `oneOf` 一致），且 `file` 为工作区内相对路径（绝对路径仅用于只读 skill 目录脚本，见 `resolvePythonFile`）。
- `bash.command`/`pip.packages`：内联示例与约束（`packages` 已有示例，补"至少一个、可带版本约束"）。
- 其余参数（xizhi `path`、`include_hidden`、`depth`、`pattern`；webfetch `method`/`headers`）保持现有良好描述，按规则集微调（默认值内联，如 `include_hidden` "Defaults to false"已存在）。

## Risks / Trade-offs

- **[收敛 luban cross-rule 削弱 defense-in-depth？]** → 不削弱：系统提示词仍是权威源且 `luban_read_skill` 保留最简指针；`xizhi_*` 的 `validatePath` + Landlock 不依赖文案。属可接受的"去重"，非"降级"。若未来系统提示词 Skills 段落被移除，`luban_read_skill` 指针仍兜底。
- **[描述变长增加 token 开销]** → 执行类工具描述因补输出契约 + 反模式明显变长（约 +80 词），但这是"模型最需要先知道的信息"，且每请求只发一次；收益（减少误调/乱填）大于 token 成本。
- **[`invoke_*` 导出描述改动 `agent` 包公开 API]** → 新增导出符号，向后兼容；`mcp.go` 复用后行为不变。
- **[描述断言测试耦合具体措辞]** → 测试断言"包含关键短语"（如 `truncated`、`64KB`、`xizhi_*`）而非整句，给后续润色留余地，避免改个词就红。

## Migration Plan

- 纯文案 + 两处小重构，无 DB/API/协议变更。
- 部署：重新构建 `bin/blowball` 重启即可；模型在下一请求即看到新描述。
- 回滚：还原各 `register.go`/`schemas.go`/`tools.go`/`mcp.go`/`render.go` 文案与重构即可，无副作用需清理。

## Open Questions

- 无。所有改动均为文案/收敛/去重，默认行为与现有契约一致；MCP 远端工具描述明确不在范围。
