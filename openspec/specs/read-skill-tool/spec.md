# read-skill-tool Specification

## Purpose

`read_skill` 已被 `luban_read_skill` 替代。本规格保留 skill 大小限制要求，该限制现由 `luban_read_skill` 继承。

## Requirements

### Requirement: Skill size limits
`luban_read_skill` SHALL 拒绝读取超过配置大小上限的 skill 文件，防止 token 爆炸。

#### Scenario: Reject oversized skill
- **WHEN** `SKILL.md` 大小超过上限（默认 500KB）
- **THEN** `luban_read_skill` 返回错误，不加载内容
