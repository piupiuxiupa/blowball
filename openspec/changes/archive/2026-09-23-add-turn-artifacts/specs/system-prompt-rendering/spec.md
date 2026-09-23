# system-prompt-rendering Delta

## MODIFIED Requirements

### Requirement: Workspace file output convention and tmp cleanup guidance
`RenderSystemPrompt` 渲染的系统提示词 SHALL 包含一段工作空间文件输出规范（位于 `renderWorkspaceConvention()` 约定段，与既有“相对路径/沙箱 `/tmp` 映射到 `./tmp/`”约定并存），明确以下指引供模型遵循：

- **临时产物入 `tmp/`**：探索性计算、调试转储、测试脚手架等非最终交付物的中间文件 SHALL 写入 `tmp/`。
- **交付物入工作空间并归组**：最终交付物 SHALL 写入工作空间（非 `tmp/`），按主题/任务归入有意义的目录，相关联的多个文件 SHALL 放进同一目录而非散落各处。
- **`tmp/` 及时清理**：`tmp/` 为临时草稿区，Agent SHALL 在草稿完成其使命后及时清理；清理 SHALL 优先使用 `xizhi_delete`，当该工具不可用时可用 `bash rm`。
- **禁止 `tmp/` 作交付物路径**：Agent 不得把 `tmp/` 路径作为交付物路径交给用户（因为 `tmp/` 内容是临时的、会被清理）。
- **交付物链接语法**：Agent 在最终回复中向用户提及交付物文件时，SHALL 使用产物链接语法 `[显示名](blowball://workspace/<工作区相对路径>)`；显示名建议使用文件名；链接 SHALL 只用于最终交付物，且 SHALL NOT 携带任何查询参数（版本钉定由系统完成）。

#### Scenario: Convention distinguishes temporary from deliverables
- **WHEN** 系统提示词被渲染
- **THEN** 约定段指示临时/中间产物写入 `tmp/`、最终交付物写入工作空间（非 `tmp/`）

#### Scenario: Convention directs grouping related files
- **WHEN** 系统提示词被渲染
- **THEN** 约定段指示相关联的文件放进同一目录、按主题/任务归组

#### Scenario: Convention directs timely tmp cleanup via xizhi_delete
- **WHEN** 系统提示词被渲染
- **THEN** 约定段指示 Agent 在草稿完成使命后及时清理 `tmp/`，并优先使用 `xizhi_delete`（不可用时 `bash rm`）

#### Scenario: Convention forbids handing tmp paths as deliverables
- **WHEN** 系统提示词被渲染
- **THEN** 约定段指示不得把 `tmp/` 路径作为交付物路径交给用户

#### Scenario: Convention coexists with relative-path and tmp-mapping guidance
- **WHEN** 系统提示词被渲染
- **THEN** 输出规范与既有“`xizhi_*` 用相对路径”“沙箱 `/tmp` 映射到 `./tmp/`”指引并存于同一约定段，互不取代

#### Scenario: Convention defines artifact link syntax
- **WHEN** 系统提示词被渲染
- **THEN** 约定段指示 Agent 在最终回复中提及交付物时使用 `[显示名](blowball://workspace/<工作区相对路径>)` 链接语法，且链接不携带查询参数

#### Scenario: Convention limits links to deliverables
- **WHEN** 系统提示词被渲染
- **THEN** 约定段指示产物链接仅用于最终交付物，`tmp/` 与 `.blowball/` 路径不得出现在产物链接中
