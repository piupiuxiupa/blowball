## Why

当前每轮 turn 结束的持久化是一次 FS 读改写 + 一次 MySQL 多值 INSERT + 一次 `UpdateSessionTime` 的同步串行路径：在 `storage.workspace.backend: shared` 模式下 FS 读改写是网络文件系统往返，MySQL 同步写在高峰期形成写入压力，而 FS 温层的读改写/回填/孤儿语义维护成本高。Redis 本就是写路径必经之路，仓库内已有 `llm_raw:buffer` 的 write-behind 已验证模板（`internal/llmraw`），把消息持久化改造成"Redis 优先写入 + 后台 flusher 分批幂等入库"可以将每轮持久化成本压缩到一次 Redis pipeline，同时将 MySQL 写合并批量化并整体移除 FS 温层。

## What Changes

- **写路径**：`SaveMessagesBatch` 不再同步写 MySQL、不再写 FS，改为单个 Redis pipeline 双写：`RPUSH msgs:{sid}`（读热层，保留 24h TTL）+ `RPUSH msgs:buffer`（全局入库队列，无 TTL）。`msg_time`/`msg_index` 仍在持久化时定死，乱序入库对读序无害。
- **Redis 失败降级**：pipeline 失败时降级为同步直写 MySQL（保留"消息必赢"约定）。
- **幂等键**：新 migration 给 messages 加 `client_msg_id CHAR(36)` + UNIQUE 索引（遗留行 NULL），持久化时 mint UUID，INSERT 改 `INSERT IGNORE`。
- **后台 flusher**（新包，以 `internal/llmraw` Flusher 为模板，agent/all 角色每进程一个，多进程安全）：`LMOVE buffer→processing` 原子认领 → 幂等批量 INSERT → 成功逐条 `LREM` 确认；失败留在 processing 退避重试、永不丢弃；启动时回收 processing 残留；FK 错误（会话已删）为唯一 dead-letter 例外；成功批次顺便批量刷新 `sessions.update_time`。参数 config 化，默认 flush 间隔 1s、批阈值 100。
- **读路径**：`RecoverMessages` 删除 FS 分支；Redis miss 时先做一次同步 drain 再读 MySQL、回填 Redis（闭合回填竞态）。历史分页接口不变（接受 ≤flush 间隔的读己之写窗口）。
- **删除会话**：改为三步——①同步 drain buffer+processing（未入库消息先进 MySQL，保证归档完整）→ ②MySQL 事务（归档镜像表 + 删除 sessions 行）→ ③主动清除 Redis keys（**行为变更**：现状是刻意不清理、依赖 TTL）。
- **BREAKING — FS 温层整体移除**：`fs.Store` 收缩到 `EnsureUserDirs`（且不再创建 `sessions/` 子目录）；存量 `data/{userID}/sessions/` 不做回填迁移，任何仅存 FS、从未成功进过 MySQL 的历史孤儿消息将被丢弃（已接受的决策）；运维可自行清理该目录。
- **部署前置**：Redis 成为唯一同步层，必须开启 AOF 持久化（`appendonly yes`）；文档与启动日志提示。
- 不变项：`SaveTurnUsage` 保持独立同步调用；`turn_usage`/llmraw 机制不动；SSE 流与事件形状不动。
- 顺带修正两处既有 spec 漂移：`turn-cost-tracking` 的"同事务写 usage"（实现早已是独立调用）；`session-management` 的"orchestrator 失败不落库"（实现为部分落库，见 interrupted-turn-persistence）。

## Capabilities

### New Capabilities

- `message-write-behind`: Redis 入库队列（`msgs:buffer`/`msgs:processing` 双列表可靠队列语义）与后台 flusher 的分批幂等入库——LMOVE 认领、LREM 确认、失败重试不丢弃、FK 死信例外、启动回收、批量刷新 update_time、同步 drain 原语、配置参数与角色归属（agent/all）。

### Modified Capabilities

- `session-management`: "Three-layer message storage" 改为 Redis 优先双层写入（读缓存 + 入库队列、失败降级直写、orchestrator 失败部分落库的漂移修正）；"Message recovery with fallback" 去掉 FS 层并增加 miss 先 drain；"Message data model" 增加 `client_msg_id` 列与唯一索引；"Session list" 的 update_time 刷新时机改为 flusher 批量刷。
- `session-crud`: "Delete session" 改为 drain → MySQL 归档删除事务 → 主动清除 Redis keys 的三步流程。
- `turn-cost-tracking`: "Usage written in same transaction as message batch" 修正为独立调用（与既有实现对齐，并适配消息批次异步化后的时序）。

## Impact

- **代码**：`internal/service/{session,message,deps}.go`、`internal/store/redis/`（新 buffer/processing key 族 + LMOVE/LREM 命令）、`internal/store/mysql/message.go`（client_msg_id + INSERT IGNORE）、`internal/handler/{message_stream,session}.go`、新 flusher 包、`internal/store/fs/` 收缩、`cmd/blowball/serve.go`（flusher 装配）、`internal/config/`。
- **Migration**：`migrations/0xx_client_msg_id.sql`（加列 + UNIQUE 索引；存量库需手动应用）。
- **文档**：CLAUDE.md 持久化章节、config.example.yaml、部署要求（Redis AOF）。
- **测试**：service/redis 单测重写，`test/integration/` 的 FS fake 与持久化断言调整，新增 flusher 单测（重试/死信/回收/幂等）。
- **运维**：切换部署后存量 `data/{userID}/sessions/` 可手工清理；Redis 必须启用 AOF；api/agent 分角色部署时 flusher 仅存在于 agent/all 角色。
