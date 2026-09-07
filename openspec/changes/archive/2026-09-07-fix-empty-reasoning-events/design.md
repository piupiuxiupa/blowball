## Context

`internal/agent/openai_client.go` 的 `StreamChat` 逐帧解析 SSE chunk：

- content 分支已有判空：`delta.Content != ""` 才调用 `onToken`；
- reasoning 分支只判了 JSON 文本层（`raw != "" && raw != "null"`）。空字符串的 JSON 编码是 `""`（两个字符），能通过该判断，unmarshal 后 `rc == ""` 仍然无条件 `WriteString` + `onReasoning(rc)`。

部分 OpenAI 兼容网关在每个 content chunk 上都附带 `reasoning_content: ""`。事件序列因此变成 `T("你"), R(""), T("好"), R(""), ...`：

- `MergeEvents`（`internal/handler/event_mapper.go`）只合并相邻同类型事件，空 reasoning 事件把 token 序列切碎 → content 每 chunk 一条持久化行；
- 每个空 reasoning 事件本身也落成 `event_type='reasoning', content=''` 的行；
- 真正的 reasoning delta 通常位于流开头且连续，能正确合并成一条 —— 这就是"reasoning 一整条、content 碎片化、空 reasoning 也出一条"三个表象的共同根因。

## Goals / Non-Goals

**Goals:**

- 在流式解析入口丢弃空字符串 reasoning delta：不调用 `onReasoning`、不写入 `reasoningContent` 聚合器。
- 使相邻 token 事件恢复合并，SSE 流不再出现空 `reasoning` 帧，`messages` 表不再新增空 reasoning 行。
- 用回归测试锁定"chunk 同时携带 content 与空 `reasoning_content`"这一真实网关形态。

**Non-Goals:**

- 不在 agent 三处回调（`confucius.go` / `roundcap.go` / `subagent.go`）重复判空 —— 修复收口在客户端入口，避免同一条规则散落四处。
- 不在 `MergeEvents` 层过滤空事件 —— 合并层语义保持"只合并、不清洗"。
- 不修改表结构、不新增迁移文件。
- 不自动清理存量脏数据。

## Decisions

### D1: 修复点收口在 `openai_client.go` 的 reasoning 分支

在 `rcField` unmarshal 成功后增加 `rc != ""` 守卫（与 content 分支的 `delta.Content != ""` 对称）：仅非空时 `reasoningContent.WriteString(rc)` 并调用 `onReasoning(rc)`。

**备选与否决理由：**

- *agent 回调层判空*（confucius/roundcap/subagent 三处）：能挡住本 bug，但规则分散，未来新的 `LLMClient` 实现（或测试 fake）仍可能把空 delta 漏进 hub。
- *`MergeEvents` 丢弃空事件*：保护面最大，但让"合并层"兼职"清洗层"，且空 reasoning 帧已经污染了 SSE 流（合并层在 SSE 之后），修复不完整。

入口判空同时覆盖 SSE 与持久化两条下游路径，是最小且完备的修复点。

### D2: `LLMResponse.ReasoningContent` 聚合同样跳过空串

`WriteString("")` 本身无害，但聚合器与回调保持同一守卫可保证"resp 里有的内容 = hub 里流过的内容"这一不变量，round-cap / raw-capture / 下一轮上下文重建不会出现口径漂移。

### D3: 存量清理 SQL 独立交付，不进 `migrations/`

清理属于一次性数据修复而非 schema 演进；放入 `migrations/` 会让所有环境（包括没有此 bug 数据的新库）强制执行业务数据删除。SQL 随本 change 的交付说明单独提供给运维，按需手工执行（先 SELECT 复核、事务内 DELETE、可回滚策略见 Risks）。

### D4: 回归测试直接打在 `StreamChat` 层

测试构造 SSE 流：reasoning chunk → 多个 `content + reasoning_content:""` 混合 chunk → 收尾帧。断言 `onReasoning` 仅收到非空 delta、`onToken` 收到全部 content、`resp.ReasoningContent` 不含空串注入。这比在 `MergeEvents` 层加测试更靠近根因，且能同时锁定聚合器行为。

## Risks / Trade-offs

- [某个网关用 `reasoning_content: ""` 作为"思考阶段开始/结束"标记] → 语义上空串不携带任何思考内容，丢弃不损失信息；真正的边界由 delta 到达顺序表达，不受影响。
- [存量空行仍在，历史 turn 的 token 碎片化无法由本修复逆转] → D3 的手工 SQL 只删空 reasoning 行；碎片化 token 行属历史事实，保留不重建（msg_index/client_msg_id 语义依赖原始行序）。
- [清理 SQL 误删] → SQL 先以 SELECT 形式给出复核查询；DELETE 限定 `event_type='reasoning' AND content=''`，并在事务中执行。

## Migration Plan

1. 合并修复代码并部署：新 turn 不再产生空 reasoning 事件/行。
2. 按需执行随交付提供的独立清理 SQL（不在代码仓库内）。
3. 回滚：revert 代码即可恢复旧行为；已执行的清理 SQL 不可自动回滚（删除的均为空内容行，无信息损失）。

## Delivery: 存量清理 SQL（独立执行，不入 migrations/）

```sql
-- ① 复核：确认将被删除的行（应全部是 content 为空字符串的 reasoning 行）
SELECT id, session_id, msg_time, agent, msg_index, trace_id
FROM messages
WHERE event_type = 'reasoning'
  AND content = ''
ORDER BY session_id, msg_time, msg_index;

-- ② 统计影响面
SELECT session_id, COUNT(*) AS empty_reasoning_rows
FROM messages
WHERE event_type = 'reasoning' AND content = ''
GROUP BY session_id;
```

```sql
-- ③ 事务内删除
START TRANSACTION;

DELETE FROM messages
WHERE event_type = 'reasoning'
  AND content = '';

-- 核对删除行数与 ② 的合计一致后提交；不符则 ROLLBACK
COMMIT;
```

删除条件精确限定 `event_type='reasoning' AND content=''`，只命中本 bug 产生的空行；
`msg_index` 留下的空洞无害——历史重建按 `(msg_time, msg_index)` 排序，跳号不影响顺序。

## Open Questions

（无）
