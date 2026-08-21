# Design: title-generation-cadence

## Context

现行链路:`SendMessage` → turn 运行(可能数分钟)→ `persistEvents` 内 `isFirstTurn`(= `len(prior) == 0`,`message_stream.go:253`)成立时 `go titleSvc.GenerateTitle(saveCtx, sessionID, req.Content, assistantContent)`(`message_stream.go:554`),其中 assistantContent 由 Confucius 的 token 事件拼接。`TitleService.generate` 触发时 `GetTitle` 早退 manual;`UpsertTitle` 无条件覆盖并把 `is_manual` 拍回 FALSE(`mysql/title.go:17-24`)。

约束:一个会话同一时刻仅一个 turn(`SESSION_BUSY` 互斥);turn 结束才持久化该 turn 的用户消息;`GenerateTitle` 自建 background context(HTTP 取消不影响)+ panic recover;`interrupted-turn-persistence` 规格要求中断的首轮 turn 也用部分 assistant 内容触发标题。

## Goals / Non-Goals

**Goals:**

- 标题在消息发出后数秒内可得(与 turn 并行生成),失败/中断的 turn 也有标题。
- 标题随对话推进按 1、4、7…条用户消息的节奏刷新,成本有界(每 3 条消息至多一次廉价 title 调用)。
- manual 标题在任何时序下不被 AI 生成覆盖(SQL 原子守卫)。

**Non-Goals:**

- 不改 titles 表结构、PATCH 手动改题端点、sanitize 规则、`title_model` 选型。
- 不做 per-session singleflight/取消旧生成(let-race)。
- 不改 SSE 事件面与 API 契约(前端自行 refetch 感知新标题)。
- 不把 assistant 回复纳入标题输入(发送时不存在;如后续想加"turn 结束后精修"再议)。

## Decisions

### D1:触发点 = claim 成功后、orchestrator 启动前,fire-and-forget

位置紧随 session-run claim 成功之后:409 `SESSION_BUSY` 的请求不触发(会话忙,不值得白生成一次);claim 失败降级为无互斥执行的场景(既有约定)照常触发。goroutine 沿用 `GenerateTitle` 自带的 detached context 与 recover——HTTP 断连、turn 失败、进程内 panic 均不影响。**备选**:claim 之前触发被否——被 409 拒绝的请求也消耗一次 LLM 调用;persist 阶段触发(即仅挪早到 turn 后)被否——用户明确要发送即得。

### D2:序号判定 n % 3 == 1,n = 已持久化 user 行数 + 1

计数基准:`RecoverMessages` 返回的 `prior` 中 `role == user` 的行数 + 1(本条)。确定性成立——turn N-1 结束才持久化其用户消息,而 `SESSION_BUSY` 保证发送时序串行,`prior` 恰为消息 1..n-1 的完整集合。compaction 不影响计数(messages 表全量保留,恢复的历史不因 compaction 缺行)。节流常量 3 与"首条必触发"由 `n % 3 == 1` 一式表达(n=1,4,7…),不引入可配置项(无运维调参需求,先硬编码)。**备选**:按 turn 完成数在 persist 阶段计数被否——与 D1 的发送时机矛盾;引入 `titles.refresh_interval` 配置被否——YAGNI。

### D3:输入 = 首条用户消息 + 本次触发消息

`GenerateTitle` 签名从 `(ctx, sessionID, userMsg, assistantMsg)` 改为 `(ctx, sessionID, firstUserMsg, currentUserMsg)`(两个显式参数,不让 service 自己查历史——handler 已持有 `prior`,首条 user 行零成本可得;n=1 时两参数同值)。prompt 组装改为双 User 段(标注"首条提问"/"最新提问"),system prompt 不变。sanitize 的空回退取**本次**消息前 20 runes。**备选**:(a) 仅本次消息——标题随话题每 3 条漂移一次,丢失会话主题锚点;(c) 压缩历史——最贵且收益边际。首条+本次在"稳定锚点 + 最新话题"间取得平衡,且是零额外 I/O 的选项。

### D4:manual 守卫下沉到 upsert(原子)

```sql
INSERT INTO titles (session_id, title, trace_id, is_manual)
VALUES (:session_id, :title, :trace_id, FALSE)
ON DUPLICATE KEY UPDATE
    title     = IF(is_manual, title, VALUES(title)),
    trace_id  = IF(is_manual, trace_id, VALUES(trace_id)),
    is_manual = is_manual
```

触发时的 `GetTitle` 早退保留(省一次 LLM 调用),但它只裁剪成本、不保证正确性;正确性由 SQL 层保证:即使早退检查与 upsert 之间用户设置了 manual 标题,AI upsert 也不覆盖、不解除 manual。`trace_id` 同步受守卫(标题没被采纳时,生成它的 trace 也不该留名)。`UpsertTitleManual` 不加守卫——manual 永远胜是既有语义。**备选**:upsert 前再读一次 + 条件写被否——两步仍然有窗口;`UPDATE ... WHERE is_manual = FALSE` + 影响行数判断被否——与 INSERT-or-UPDATE 的 upsert 形状不匹配,`IF` 变体保持单语句单往返。

### D5:中断路径的需求随旧触发点一并移除

`interrupted-turn-persistence` 的 "Title generation runs on interrupted first turn" 描述的是"turn 结束后触发"世界里的补偿(中断的首轮也要有标题)。发送时触发后,标题在 turn 开始前已生成,中断/失败路径天然有标题,该需求整体移除(非修改——其描述的机制不复存在)。`persistEvents` 中为标题拼接 assistantContent 的循环一并删除。

## Risks / Trade-offs

- [标题质量] 无 assistant 回复参与,首轮标题只基于提问 → 提问通常足以命名会话;第 4、7…次刷新时输入仍只有用户侧文本 → 接受(成本与时效的既定取舍,design D3 记录了加精修的扩展位)。
- [成本] 每会话每 3 条消息多一次 title LLM 调用 → `title_model` 通常为廉价条目;节流常量就是为此而设。
- [let-race] 极端时序下两次 AI 生成竞写 → last-write-wins,均为 AI 值、manual 不变量由 D4 保证;无正确性影响。
- [前端感知] turn 进行中标题变化,前端不 refetch 则显示旧标题 → 前端 repo 在发送后/turn 中 refetch 列表或详情(契约未变,`GET /sessions/:session_id` 即为此而设);后端不做推送。
- [fake store 漂移] 测试 fake 的 `UpsertTitle` 若按旧语义实现(无条件覆盖)会漏掉守卫回归 → fake 直接复用/镜像新 SQL 语义,补一条"manual 行不被 AI upsert 改写"的单测钉住。

## Migration Plan

纯服务端行为变更,无配置、无 DB 迁移。部署即生效;回滚即还原二进制。存量会话的旧标题不受影响(AI 覆盖语义对既有 `is_manual = FALSE` 行与从前一致)。

## Open Questions

(无——节流节奏、输入形状、let-race、SQL 守卫均在探索阶段拍板;是否在 turn 结束后追加"精修"留作未来独立变更,见 D3/Non-Goals。)
