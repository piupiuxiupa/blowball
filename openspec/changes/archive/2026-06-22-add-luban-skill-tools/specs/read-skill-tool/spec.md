## REMOVED Requirements

### Requirement: read_skill tool registration
**Reason**: `read_skill` 被 `luban_read_skill` 替代，luban 工具系列专门负责 skills 目录操作，并与 xizhi 文件工具明确区分。

**Migration**: 在 `config.yaml` 的 agent `tools` 列表中，将 `read_skill` 替换为 `luban_list_skills`、`luban_read_skill`、`luban_install_skill`。system prompt 会自动引导模型使用 luban 工具读取 skill。

### Requirement: Skill lookup by name
**Reason**: skill 读取逻辑迁移到 `luban_read_skill`，由 `internal/tool/luban/read.go` 实现。

**Migration**: 所有 agent 调用从 `read_skill(name)` 改为 `luban_read_skill(name)`，行为保持一致（用户优先、回退全局、返回 markdown body）。

### Requirement: Error handling for unknown skill
**Reason**: 未知 skill 错误处理由 `luban_read_skill` 继承。

**Migration**: 无需额外迁移；`luban_read_skill` 在 skill 不存在时返回明确错误。

## MODIFIED Requirements

### Requirement: Skill size limits
`luban_read_skill` SHALL 拒绝读取超过配置大小上限的 skill 文件，防止 token 爆炸。

#### Scenario: Reject oversized skill
- **WHEN** `SKILL.md` 大小超过上限（默认 500KB）
- **THEN** `luban_read_skill` 返回错误，不加载内容
