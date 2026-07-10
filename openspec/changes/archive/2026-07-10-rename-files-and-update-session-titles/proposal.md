## Why

当前用户无法修改自动生成的会话标题，也无法在 UI 中重命名工作空间中的文件或目录。为了支持后续前端实现会话标题编辑和文件树重命名，需要先暴露两个后端接口，并保证用户手动设置的标题不会被异步 AI 标题生成覆盖。

## What Changes

- 新增 `PATCH /api/v1/sessions/:session_id` 接口，允许已鉴权用户修改自己会话的标题。
- `titles` 表新增 `is_manual` 字段，用于标记标题是否由用户手动设置。
- AI 异步标题生成逻辑在写入前检查 `is_manual`：若为 `TRUE` 则跳过，不覆盖用户手动标题。
- 新增 `PUT /api/v1/workspace/files/*path` 接口，支持重命名文件或目录。
- 重命名时若目标路径已存在（文件或目录），返回 409 错误，不执行覆盖。
- 更新 `api/openapi.yaml`，加入上述两个端点。
- 仅修改后端，不改动前端。

## Capabilities

### New Capabilities

- `session-title-update`: 会话标题的手动更新能力，包含 `is_manual` 标记和防 AI 覆盖策略。
- `workspace-file-rename`: 工作空间文件/目录重命名能力，含路径校验与目标存在性检查。

### Modified Capabilities

- `session-management`: 自动标题生成需遵守 `is_manual` 标记，不再覆盖用户手动标题。

## Impact

- 数据库：新增 migration 修改 `titles` 表结构。
- 后端代码：`internal/store/mysql/title.go`、`internal/service/title.go`、`internal/handler/session.go`、`internal/handler/workspace.go`、`internal/handler/router.go`、`api/openapi.yaml`。
- 测试：对应 handler、service、integration 测试。
- 前端：无影响，后续再接入。
