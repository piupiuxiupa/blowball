## Why

当前 turn 产物用 workspace mtime 扫描归因。同一用户的多个 session 共享同一个 workspace，因此并发 turn 会把其他 session 在本 turn 开始后写入的文件误加入自己的 artifact 列表。最终回复中的 `blowball://workspace/...` 链接已经是模型对交付物的显式声明，应成为唯一归因来源。

## What Changes

- 删除 turn 末 workspace mtime 扫描；不再以 turn 开始时间判断产物归属。
- 在 turn 结束前，从当前 turn 的顶层 assistant 内容中提取 `blowball://workspace/<path>` 链接。
- 对提取到的路径做解码、校验、去重，只保留存在且可读取的常规非空文件。
- 只为链接声明的文件创建版本快照和 artifact 事件；模型未链接的文件不会出现在 artifact 面板。
- 保持 artifact 事件按既有契约进入历史消息记录，作为本变更的回归验证点。
- 保留既有快照降级、去重和版本读取行为。
- 强化系统提示词约定：最终交付物必须使用产物链接，否则不会进入 artifact 记录。
- 明确同一 path 的并发写入仍是共享 workspace 写冲突，本变更不解决文件内容竞争。

## Capabilities

### New Capabilities

- 无

### Modified Capabilities

- `turn-artifacts`: 产物候选从“mtime 不早于 turn 开始时间的文件”改为“当前 turn 最终回复中的产物链接”；并发 session 不再互相污染 artifact 列表。
- `system-prompt-rendering`: 工作空间输出约定从“提到交付物时使用链接”强化为“每个最终交付物必须链接”，并说明未链接文件不会进入 artifact 记录。

## Impact

- `internal/artifact`：删除 mtime `Detect`，新增链接提取与路径校验；`FinalizeTurn` 输入从时间 watermark 改为回复内容。
- `internal/handler`：在 done 前把当前 turn 顶层 assistant 内容传给产物 finalization hook；artifact 事件顺序不变。
- `internal/prompt`：强化交付物链接要求。
- 测试：覆盖链接提取、非法/重复链接、无链接输出、并发 session 隔离，以及既有快照行为。
- 无 HTTP API、数据库 schema 或外部版本服务契约变更。
