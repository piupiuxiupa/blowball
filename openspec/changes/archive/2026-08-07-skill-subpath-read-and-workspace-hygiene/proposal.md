## Why

技能（skill）经常不止一个 `SKILL.md`，其子目录中还带有示例、参考等关联 markdown 文档，但 `luban_read_skill` 目前只能读取与技能名匹配的那个 `SKILL.md`，无法读取技能目录树内的其他 `.md` 文件。同时，Agent 在工作空间中生成文件时缺乏统一的输出规范——临时文件与交付物混放、相关文件散落各处、`tmp/` 草稿目录无人清理越积越多。本变更让 Agent 既能读全一份技能，也能把工作空间打理干净。

## What Changes

- **`luban_read_skill` 支持按相对路径读取技能树内任意 `.md` 文件**：新增可选 `path` 参数。省略 `path` 时行为不变（读取该技能的 `SKILL.md`，向后兼容）；提供 `path` 时，将其解析为相对于该技能目录根的相对路径，经“限制在技能目录根内”的路径校验后，仅当目标是 `.md` 文件时读取（有 frontmatter 则剥离、无则原样返回），并复用现有大小上限。
- **新增 `xizhi_delete` 文件删除工具**：Xizhi 工具集目前只有 read/write/modify/list/tree/glob，缺少删除原语。新增 `xizhi_delete`（受 `tools.xizhi.delete.enabled` 开关控制，沿用现有 xizhi 路径校验、保留目录 `.blowball` 拒绝规则），使 Agent 拥有清理自身草稿文件的能力——无论 `bash` 执行器是否启用。
- **系统提示词新增工作空间文件输出规范与 `tmp/` 清理指引**：在 `renderWorkspaceConvention()` 中明确——临时/中间产物写入 `tmp/`，最终交付物写入工作空间并按主题归组、相关文件放进同一目录；`tmp/` 为临时草稿区，Agent 应在草稿完成使命后及时用 `xizhi_delete`（或在无 `xizhi_delete` 时用 `bash rm`）清理，且不得把 `tmp/` 路径作为交付物路径交给用户。

## Capabilities

### New Capabilities
- `xizhi-delete-files`: 新增 `xizhi_delete` 工具——在用户工作空间内删除文件或目录，沿用既有 xizhi 路径校验（拒绝绝对路径 / `..` 越界 / 符号链接逃逸 / 保留目录 `.blowball`），受 `tools.xizhi.delete.enabled` 开关控制。

### Modified Capabilities
- `luban-skill-tools`: `luban_read_skill` 新增可选 `path` 参数，支持读取技能目录树内任意 `.md` 文件（限制在技能目录根内、仅 `.md`、复用大小上限），并相应更新工具描述；`luban_read_skill` 名称仍必须是简单标识符，`path` 才是文件路径。
- `system-prompt-rendering`: `renderWorkspaceConvention()` 新增工作空间文件输出规范（临时→`tmp/`、交付物→工作空间并按主题归组、相关文件同目录）与 `tmp/` 及时清理指引（使命结束后用 `xizhi_delete` 清理，禁止把 `tmp/` 路径作为交付物）。

## Impact

- **`internal/tool/skill/skill.go`**：`Loader` 新增按“技能名 + 技能内相对路径”读取 `.md` 文件的能力，含限制在技能目录根内的路径校验（拒绝绝对路径 / `..` 越界 / 符号链接逃逸）。
- **`internal/tool/luban/read.go`、`internal/tool/luban/register.go`**：`luban_read_skill` 注册新增可选 `path` 参数并更新工具描述。
- **`internal/tool/xizhi/`**：新增 `delete.go`（删除文件/目录实现）、`register.go` 注册 `xizhi_delete`（`NameDeleteFile` 常量、`schemaDelete`、受 `cfg.Delete.Enabled` 控制），`validate.go` / 路径校验复用现有逻辑。
- **`internal/config/config.go`**：`XizhiConfig` 新增 `Delete XizhiToolConfig` 字段（`tools.xizhi.delete.enabled`，默认与现有 `list_files`/`tree`/`glob` 一致的开关语义）。
- **`internal/prompt/render.go`**：`renderWorkspaceConvention()` 扩充输出规范与 `tmp/` 清理指引文案。
- **`config.example.yaml`**：Chongzhi 工具列表加入 `xizhi_delete`；`tools.xizhi` 补充 `delete` 开关示例。
- **测试**：新增/扩展 `internal/tool/luban/`、`internal/tool/xizhi/`、`internal/prompt/` 的单元测试，覆盖 `path` 越界、符号链接逃逸、`.md` 限定、删除保留目录拒绝、清理指引渲染等。
- **无 API/DB schema 变更**，无破坏性变更（`luban_read_skill` 的 `path` 可选、`xizhi_delete` 默认开关控制）。
