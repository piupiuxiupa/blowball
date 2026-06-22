## Why

当前 agent 在 system prompt 中看到 skills 后，会尝试用 `xizhi_read_file` 读取 skill 文件，但 xizhi 工具被限制在 `data/{userID}/workspace/` 内，无法访问同级的 `skills/` 目录，导致路径验证失败。同时用户无法通过对话让 agent 从外部 URL 安装新的 skill 集合到自己的工作空间。需要一套专门操作 skills 目录的工具，并支持从 URL 整体安装 skill 集合。

## What Changes

- 新增 `internal/tool/luban/` 工具包，提供三个内置工具：
  - `luban_list_skills`：递归扫描全局 `skills/` 和当前用户 `data/{userID}/skills/`，返回合并后的可用 skill 列表（用户覆盖全局）。
  - `luban_read_skill(name)`：按名称读取 SKILL.md 正文（剥离 YAML frontmatter），优先用户目录，找不到时回退全局。
  - `luban_install_skill(url, ?name)`：从 URL 安装 skill 集合到用户 `skills/` 目录，支持 GitHub repo 整体克隆，也支持直接下载单个 SKILL.md。
- 将 `skill.Loader.discover` 从单层扫描改为递归扫描，支持 `superpowers/skills/{name}/SKILL.md` 这类嵌套结构。
- system prompt 中的 Skills catalog **仅注入全局 skills**（按 `agent.skills` 过滤）；用户 skills 由模型通过 `luban_*` 工具动态发现。
- system prompt 明确禁止模型使用 `xizhi_*` 工具访问 skills 目录。
- `config.yaml` 中 agent 的 `tools` 列表移除 `read_skill`，替换为 `luban_list_skills`、`luban_read_skill`、`luban_install_skill`；`agent.skills` 只声明需要注入系统提示词的全局 skills。
- **BREAKING**: `read_skill` 工具不再被 agent 使用，由 `luban_read_skill` 替代。

## Capabilities

### New Capabilities
- `luban-skill-tools`: 定义 luban 系列工具的接口、行为、错误处理及安全边界（list / read / install）。

### Modified Capabilities
- `agent-skill-configuration`: 
  - skill 发现从单层扫描改为递归扫描。
  - system prompt 只注入全局 skills，不再把用户 skills 预注入 catalog。
  - agent `skills` 配置仅用于声明全局 skills。
- `read-skill-tool`: 该能力被 `luban-skill-tools` 覆盖，原有 `read_skill` 工具保留注册但不再被 agent 配置使用（或后续移除）。

## Impact

- 新增包：`internal/tool/luban/`（register.go、list.go、read.go、install.go、validate.go）。
- 修改：`internal/tool/skill/skill.go`（递归发现、区分全局/用户查询）。
- 修改：`internal/prompt/render.go`（更新 Skills 段落文案，加入 luban 工具使用说明和 xizhi 禁止说明）。
- 修改：`internal/agent/orchestrator.go`（system prompt 只注入全局 skills；per-request registry 复制 luban 工具）。
- 修改：`cmd/server/main.go`（注册 luban 工具；调整 `needsReadSkill` 检测逻辑）。
- 修改：`config.yaml`（更新 agent tools 和 skills 配置）。
- 依赖：新增 `git` 命令用于克隆 skill 集合；新增 HTTP 下载逻辑复用 webfetch 能力或标准库。
