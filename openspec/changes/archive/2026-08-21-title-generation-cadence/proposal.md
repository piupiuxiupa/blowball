# Proposal: title-generation-cadence

## Why

现状标题生成挂在**首轮 turn 结束并持久化之后**(`message_stream.go:554`,`isFirstTurn` 判定),长 agent turn 意味着侧边栏要等几分钟才有标题;且每会话只生成一次,话题迁移后标题停在开场。同时 `UpsertTitle` 是无条件覆盖并把 `is_manual` 拍回 FALSE——AI 生成在 flight 中用户手动改标题会被冲掉并永久解除 manual 保护(现有竞态,目前窗口仅首轮一次,扩大触发频率前必须先堵上)。本变更把触发点移到**用户发送消息时**(与 turn 并行)、按提问次数节流再生成,并在 SQL 层给 manual 标题加原子守卫。

## What Changes

- **触发时机**:`POST /messages` 的 session-run claim 成功之后、orchestrator 启动之前,fire-and-forget 触发标题生成(与 turn 并行;409 `SESSION_BUSY` 不触发;沿用 detached context + panic recover 的 fire-and-forget 惯例)。**删除**旧的 turn 结束后 `isFirstTurn` 触发路径(含 `persistEvents` 里为标题拼接 assistantContent 的代码)。
- **触发节流(按提问次数)**:以会话内用户消息序号 n(=已持久化 user 行数 + 1,1 基)判定,n % 3 == 1 时触发——即第 1、4、7、10…条用户消息触发,首条必触发,其后每 3 条刷新一次。
- **再生成输入**:标题 prompt 输入改为**首条用户消息 + 本次触发消息**(两个"会话主题锚点 + 最新话题"信号;handler 已持有 `prior` 历史,零额外读)。prompt 不再包含 assistant 内容。
- **manual 双层守卫**:① 触发时 `GetTitle` 早退(已有行为,`is_manual` 跳过 LLM 调用);② `UpsertTitle` 的 SQL 改为原子守卫——`ON DUPLICATE KEY UPDATE title = IF(is_manual, title, VALUES(title)), trace_id = IF(is_manual, trace_id, VALUES(trace_id)), is_manual = is_manual`,消除"AI upsert 冲掉手动标题"的竞态窗口。`UpsertTitleManual`(手动路径)不变——manual 永远胜。
- **并发碰撞策略**:同会话两次生成在 flight 相撞时 let-race(last-write-wins,均为 AI 写、`is_manual` 守卫不变量保持);不加 singleflight(碰撞窗口被 `SESSION_BUSY` 的 turn 串行化压到极小,代价不值得)。
- 失败降级、sanitize(20 runes 截断、空回退取用户消息前 20 runes)、`title_model` 选型均不变。

## Capabilities

### New Capabilities

(无。)

### Modified Capabilities

- `session-management`:“Auto generate session title” 需求改为发送时触发 + 提问次数节流 + 新输入形状 + `UpsertTitle` 原子 manual 守卫。
- `interrupted-turn-persistence`:移除 "Title generation runs on interrupted first turn" 需求——发送时触发意味着标题在 turn 开始前已生成,中断路径不再需要(也无需)触发标题。

## Impact

- **handler**:`internal/handler/message_stream.go`(新增发送时触发点与序号判定;删除 `persistEvents` 的 `isFirstTurn` 标题分支与 assistantContent 拼接)。
- **service**:`internal/service/title.go`(`GenerateTitle` 签名改为接收首条+本次消息;prompt 组装调整)。
- **store**:`internal/store/mysql/title.go`(`upsertTitleSQL` 原子守卫)+ `internal/service/title_test.go`、集成测试的 fake store 同步。
- **前端**(blowball-frontend repo,非本变更代码):turn 进行中标题会更新,列表/详情需在发消息后 refetch;API 契约无变化。
- 无 DB 迁移(`titles` 表结构不变,仅 upsert 语句变化)。
