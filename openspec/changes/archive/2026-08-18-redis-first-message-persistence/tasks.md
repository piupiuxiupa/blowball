## 1. 数据模型与 Migration

- [x] 1.1 `model.Message` 增加 `ClientMsgID` 字段（JSON tag `client_msg_id`），确认序列化兼容（读侧容忍旧行无此字段）
- [x] 1.2 新增 `migrations/012_client_msg_id.sql`：messages 加 `client_msg_id CHAR(36) NULL` + UNIQUE 索引 `uk_messages_client_msg_id`
- [x] 1.3 `internal/store/mysql/message.go`：`AppendMessage`/`AppendMessages` 的 INSERT 列表加入 client_msg_id 并改 `INSERT IGNORE`；`ListMessages`/`ListMessagesPaged` SELECT 列表加入该列

## 2. Redis 入库队列 key 族

- [x] 2.1 `internal/store/redis/` 新增消息队列方法：`PushMessageBuffer`（RPUSH `msgs:buffer`，无 TTL）、`MoveMessageToProcessing`（LMOVE 单条）、`RemoveFromProcessing`（LREM by value）、`LenMessageBuffer`、`RecoverProcessingToBuffer`（启动回收：将 processing 残留按序推回 buffer 头并清空 processing）
- [x] 2.2 `internal/service/deps.go` 的 `RedisStore` 接口扩展队列方法；`SessionService` 持久化路径改为单 pipeline 双写 `msgs:{sid}` + `msgs:buffer` 的原子封装（新增 `AppendMessagesDual` 类方法）
- [x] 2.3 单测：双写 pipeline、LMOVE/LREM 语义、无 TTL 断言（复用既有 redis 单测形态）

## 3. flusher 包（以 internal/llmraw 为模板）

- [x] 3.1 新包（如 `internal/msgflush`）定义窄端口：Buffer（认领/确认/回收/长度）、MessageStore（批量幂等插入 + 批量刷 update_time），生产接线 `*redis.Store` / `*mysql.Store`，测试用内存 fake
- [x] 3.2 认领-插入-确认主循环：每轮 LMOVE 认领 ≤ flush_batch_size 条 → `INSERT IGNORE` 批量插入 → 成功逐条 LREM 确认 → 批内 distinct session 各刷一次 `update_time`（失败仅记日志）
- [x] 3.3 失败重试：插入失败时记录留在 processing，1s 退避后重试；FK 错误（MySQL 1452）按死信 LREM + ERROR（含记录摘要）处理，其余错误永不丢弃
- [x] 3.4 启动回收：Start 前将 `msgs:processing` 残留按序推回 `msgs:buffer`
- [x] 3.5 触发与停机：interval ticker + 队列长度 ≥ flush_batch_size 提前触发 + 优雅停机有界 final drain（复用 llmraw Flusher 的 stop/done/started 形态）
- [x] 3.6 同步 drain 原语：一次性排空 buffer+processing 并入库（有界超时），供删除流程与读 miss 复用
- [x] 3.7 单测：正常批量、多消费者不重不丢（fake 上模拟交错）、失败重试保留、FK 死信、崩溃回收、幂等重复插入仅一行、final drain 有界

## 4. 配置

- [x] 4.1 `internal/config`：新增顶层 `messages` 块（`flush_interval` 默认 1s、`flush_batch_size` 默认 100），非正数值加载期拒绝；`config.example.yaml` 补注释示例
- [x] 4.2 flusher 常量（错误退避 1s）与 config 参数接线

## 5. 服务层改造

- [x] 5.1 `SessionService.SaveMessagesBatch`：mint `client_msg_id`（每条）→ Redis pipeline 双写 → 成功即返回；pipeline 失败降级同步直写 MySQL（幂等 INSERT + update_time），记错误日志；删除 FS append 与同步 MySQL 常规写
- [x] 5.2 `MessageService.RecoverMessages`：删除 tryFS/writeFSFromRaws；Redis miss → 同步 drain → MySQL 读取 → SetMessages 回填
- [x] 5.3 `SessionService.DeleteSession`：①同步 drain → ②既有 MySQL 归档+删除事务（移除 FS 文件删除步骤）→ ③`DEL msgs:{sid}`/`session:{sid}`
- [x] 5.4 `deps.go`：`FSStore` 收缩（仅保留 `EnsureUserDirs`），`SessionDeps.FS` 相应调整；`MySQLStore` 增加批量幂等插入/批量 update_time 所需方法面
- [x] 5.5 单测更新：session/message 既有测试改为双层断言（双写、降级、drain 后回填、删除三步次序）

## 6. FS 层收缩与装配

- [x] 6.1 `internal/store/fs/`：删除会话文件读写方法（WriteSession/ReadSession/DeleteSession），`userSubDirs` 去掉 `sessions/`（只建 `workspace/`），包注释与单测同步
- [x] 6.2 `cmd/blowball/serve.go`：按角色构造/启动/优雅停机 flusher（agent/all 启动，api 不构造）；接入 `messages` 配置
- [x] 6.3 启动期尽力探测 Redis `appendonly`（CONFIG GET，失败跳过），未开启打 WARN 不阻断

## 7. 集成测试与端到端

- [x] 7.1 `test/integration/`：移除/替换 FS fake 相关断言；消息流测试改为"turn 后消息可在 drain 或 flush 后从 MySQL 读到"；删除会话测试断言归档含未入库消息与 Redis key 清除
- [x] 7.2 集成用 fake flusher/队列内存实现，覆盖：双写 → flush 入库 → 降级直写 → 删除三步
- [x] 7.3 `make lint` + `make test` 全绿（注：`internal/tool/mcp` 的 `TestListTools_ReturnsLiveToolList` 为存量 flaky——在未含本变更的 pristine master 上同样复现，系其异步写回与 TempDir 清理竞态；与本变更无关。`internal/llmraw` 一次计时性 flake 复跑通过）

## 8. 文档

- [x] 8.1 CLAUDE.md：持久化章节改为 Redis-first 双层 + flusher 描述、删除流程、Redis AOF 部署前置、存量 sessions/ 目录处置说明
- [x] 8.2 migration 手动应用提示（存量库需手动跑 012），及回滚 runbook（等队列清空再回滚）记录于变更或 docs（`docs/redis-first-persistence.md`；另将 `client_msg_id` 补入 `api/openapi.yaml` 的 Message schema）
