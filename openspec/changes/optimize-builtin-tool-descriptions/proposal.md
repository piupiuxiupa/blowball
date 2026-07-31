## Why

工具描述随**每次** API 请求的 `tools[]` 数组发给模型，模型仅凭 `name + description + input_schema` 决定**何时调、怎么填参、返回值怎么用**。当前内置工具描述普遍违反"工具描述优化规则集"（5 层骨架 + R1–R10）：最严重的是**输出契约缺失**（`xizhi_read_file`/`xizhi_write_file`/`xizhi_modify_file`/`bash`/`python`/`pip_install` 都没说返回什么、是否截断、截断后怎么办），其次是**反模式缺失**（`bash`/`python` 没有引导模型"读写文件请用 `xizhi_*`，别用 `cat`/`echo`/重定向"），再就是**裸参数**（`webfetch` 的 `url` 仅写 "URL to fetch."）与**全局协作规则被重复塞进多个工具描述**（"never use `xizhi_*` to access the skills directory" 同时出现在三个 luban 工具描述和系统提示词里）。这些缺口直接表现为误调、漏调、乱填参数、误解返回值。

## What Changes

- **补齐输出契约（R2）**：为 `xizhi_read_file`/`xizhi_write_file`/`xizhi_modify_file`/`xizhi_list_files`/`xizhi_tree`/`xizhi_glob_files`、`bash`/`python`/`pip_install`、`luban_read_skill` 的描述补上"返回什么、是否截断、截断后怎么办"。重点：执行类工具（`bash`/`python`/`pip_install`）描述显式声明结果结构 `{output, exit_code, truncated}`、输出上限（默认 64KB，超限追加 `...output truncated...`）与超时（默认 bash/python 30s、pip 120s）。
- **补齐反模式（R3）**：`bash`/`python` 增加"读写/搜索工作区文件优先用 `xizhi_*` 工具，避免 `cat`/`echo`/重定向/`find`/`grep`"的让位指引；`pip_install` 增加"仅用于装依赖、解决 `ModuleNotFoundError`，不要用来跑代码"的边界。
- **修裸参数与依赖（R5/R6/R8）**：`webfetch` 的 `url` 描述改为"绝对 http(s) URL"；`python` 描述显式声明 `code` 与 `file` 互斥（schema 已有 `oneOf`，描述补一句）；executor 各参数描述内联默认值。
- **去重全局协作规则（R10）**：将"不要用 `xizhi_*` 访问 skill 目录"的权威表述收敛到系统提示词（`internal/prompt/render.go` 已有），luban 工具描述只保留最简指针，不再三处重复整句。
- **消除描述双写（H1 取向）**：`invoke_chongzhi`/`invoke_liang` 的描述当前在 `internal/agent/tools.go` 与 `internal/handler/mcp.go`（`invokeDescription`）各硬编码一份，极易漂移。改为由 `agent` 包导出单一来源，`mcp.go` 复用，使 MCP 目录与模型实际看到的 `tools[]` 永不分裂。
- 非破坏性：**不改任何工具的运行时行为、参数 schema、返回值结构或 API 契约**；只改 `Description` 文案、参数级 `description` 文案、以及上述两处收敛/去重重构。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `executor-tools`：`bash`/`python`/`pip_install` 描述 SHALL 声明结果结构 `{output, exit_code, truncated}`、默认输出上限 64KB（超限截断并标记）与超时；`bash`/`python` 描述 SHALL 包含"文件读写/搜索优先用 `xizhi_*`"的反模式；`python` 描述 SHALL 声明 `code` 与 `file` 互斥。
- `xizhi-tools`：`xizhi_read_file`/`xizhi_write_file`/`xizhi_modify_file` 描述 SHALL 声明各自的结果结构与关键失败语义（如 `modify_file` 要求 `old_content` 唯一匹配）。
- `luban-skill-tools`：`luban_read_skill` 描述 SHALL 声明返回 `SKILL.md` 的 markdown body（已剥离 frontmatter）；"不要用 `xizhi_*` 访问 skill 目录"的权威表述 SHALL 以系统提示词为唯一来源，工具描述仅保留最简指针。

> `webfetch` 与 `agent-orchestration`（`invoke_*`）的描述文案也会按规则集对齐润色，但**不新增 spec 级要求**：`webfetch` 既有"description guides error recovery"要求在重写中被完整保留；`invoke_*` 的去重是纯实现重构（模型看到的描述语义不变）。

## Impact

- 代码（仅文案 + 两处收敛/去重）：
  - `internal/tool/xizhi/register.go`（6 个工具 `Description` + 各参数 `description`）。
  - `internal/tool/webfetch/register.go`（`Description` + `url`/`method`/`headers` 参数描述）。
  - `internal/tool/executor/register.go`（`bash`/`python`/`pip_install` `Description`）与 `internal/tool/executor/schemas.go`（参数级 `description`）。
  - `internal/tool/luban/register.go`（`luban_list_skills`/`luban_read_skill`/`luban_install_skill` `Description` 收敛）。
  - `internal/agent/tools.go`（导出 `invoke_*` 描述为单一来源）与 `internal/handler/mcp.go`（`invokeDescription` 改为复用 `agent` 包导出值，删除硬编码副本）。
  - `internal/prompt/render.go`（确认/补强"不要用 `xizhi_*` 访问 skill 目录"的系统提示词权威表述）。
- 测试：新增/补充"描述包含关键契约短语"的断言（沿用 `webfetch` 既有"description guides error recovery"测试范式），覆盖执行类工具的输出上限/反模式、xizhi 结果结构、luban 的 markdown body 契约。
- 不涉及：API 契约（`api/openapi.yaml`）、DB 迁移、SSE/持久化、角色划分、MCP 远端工具描述（由远端服务器提供，不在本次范围）。
