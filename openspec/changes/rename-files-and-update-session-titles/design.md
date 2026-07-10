## Context

当前系统已具备会话的自动标题生成（`TitleService.GenerateTitle` 异步写入 `titles` 表）和工作空间文件的上传、下载、删除能力。但用户无法手动覆盖标题，也无法通过 API 重命名文件或目录。本次变更为这两个操作提供后端支持，并保证手动标题的持久性。

## Goals / Non-Goals

**Goals:**
- 提供 `PATCH /api/v1/sessions/:session_id` 接口，允许用户手动修改会话标题。
- 提供 `PUT /api/v1/workspace/files/*path` 接口，支持文件/目录重命名。
- 通过 `titles.is_manual` 字段确保手动标题不被 AI 异步生成覆盖。
- 重命名时目标路径已存在则返回 409，避免数据覆盖。
- 更新 `api/openapi.yaml` 反映新接口。

**Non-Goals:**
- 不修改前端 UI。
- 不支持跨文件系统/不同设备的 rename（依赖 `os.Rename` 的原子语义，失败时返回 500）。
- 不重做现有路由结构，仅在同一路由下新增 PUT/PATCH 方法。

## Decisions

### 1. 标题防覆盖策略：新增 `is_manual` 字段
- **选择**：在 `titles` 表新增 `is_manual BOOLEAN NOT NULL DEFAULT FALSE`。
- **理由**：字段语义直接，与现有 upsert 流程兼容；后续如需扩展“标题来源”也足够。
- **替代方案**：扩展 `titles` 的 `source` 枚举字段。当前只有“AI 生成”和“用户手动”两种来源，布尔字段更简单。

### 2. 自动生成与手动更新的 store 方法分离
- **选择**：保留 `UpsertTitle` 用于 AI 生成（`is_manual=false`），新增 `UpsertTitleManual` 用于手动更新（`is_manual=true`）。
- **理由**：两条路径的业务语义不同，分离后 `TitleService.GenerateTitle` 无需关心 `is_manual` 的细节，只在调用前做一次性判断。
- **实现**：`GenerateTitle` 先 `GetTitle`；若返回 `is_manual == true` 则直接返回，不调用 LLM 也不写入。

### 3. 文件重命名路由放在 `PUT /workspace/files/*path`
- **选择**：复用现有 `/*path` catch-all，新增 PUT 方法并在 `dispatchWorkspaceFile` 中分发到 `WorkspaceRename`。
- **理由**：与现有 GET/DELETE 的通配路由保持一致，无需引入新的 URL 模式或 suffix 解析。
- **替代方案**：`POST /api/v1/workspace/rename` 独立 action 端点。虽实现更简单，但不符合资源导向的 REST 设计。

### 4. 重命名目标存在时返回 409
- **选择**：在 `os.Rename` 之前先 `os.Stat` 目标路径，若存在则返回 409 CONFLICT。
- **理由**：用户要求“已存在同名文件或目录则不修改返回错误提醒”，明确禁止覆盖；409 是标准语义。
- **源不存在**：返回 404；路径越界：返回 403。

### 5. 会话标题更新同时 touch `sessions.update_time`
- **选择**：更新标题后执行 `UPDATE sessions SET update_time = NOW() WHERE session_id = ?`。
- **理由**：会话列表按 `update_time` 降序排列，修改标题后应让该会话排到最前。
- **替代方案**：不 touch update_time。会导致用户改完标题后列表顺序不变，体验较差。

## Risks / Trade-offs

| Risk | Mitigation |
|---|---|
| `os.Rename` 在跨设备/复杂挂载点失败 | 返回 500 并记录详细错误；当前工作空间为同一本地目录，概率极低。 |
| 手动标题后 LLM 生成仍被调用（浪费 token） | `GenerateTitle` 在 LLM 调用前检查 `is_manual`，提前返回。 |
| 并发下两个用户同时重命名到同一目标 | `os.Rename` 非原子存在性检查 + rename 之间可能产生竞态；接受极小概率的覆盖风险，或通过文件锁规避（本次不引入）。 |
| 迁移后旧行 `is_manual` 为 `FALSE`，与用户预期“旧标题可被 AI 更新”一致 | 无需回填数据。 |

## Migration Plan

1. 部署新版代码前执行 migration `009_titles_manual.sql`：
   ```sql
   ALTER TABLE titles ADD COLUMN is_manual BOOLEAN NOT NULL DEFAULT FALSE;
   ```
2. 部署后端服务。
3. 验证 `PATCH /api/v1/sessions/:session_id` 和 `PUT /api/v1/workspace/files/*path` 行为。
4. 回滚：回退代码并执行反向 migration（如需严格回滚）：
   ```sql
   ALTER TABLE titles DROP COLUMN is_manual;
   ```

## Open Questions

- 标题最大长度是否维持 20 字符？当前 `sanitizeTitle` 已做截断，手动更新复用该逻辑。
- 文件重命名是否允许跨目录？允许，`new_path` 为相对 workspace 的任意路径。
