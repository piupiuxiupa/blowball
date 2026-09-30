## 1. Store 读取面

- [ ] 1.1 在 `internal/store/mysql/subagent_run.go` 新增 `ListSubAgentChildRuns(ctx, sessionID, parentInstanceID)`：按 `session_id + parent_instance_id` 查询终态子 run 行，返回 DTO 组装所需字段（agent_instance_id、run_id、name、parent_instance_id 等），带 `logQuery` 查询日志
- [ ] 1.2 为该查询添加 MySQL 单测：正常返回、无子行空列表、跨 session 隔离（其他 session 同 parent id 不返回）

## 2. Service DTO 与组装

- [ ] 2.1 `SubAgentRunSummary` / `SubAgentRunDetail` 增加 `parent_instance_id`（omitempty），`SubAgentToolCall` 增加 `sub_agent_instance_id` / `sub_agent_name`（omitempty）
- [ ] 2.2 `ListRuns`、`GetRun` 填充 `parent_instance_id`；`GetRun` 在归属校验通过后调用 `ListSubAgentChildRuns`，按 `run_id` 建子链接映射
- [ ] 2.3 `subAgentTranscript` 组装（或其后处理）为 `spawn_subagent` 且命中映射的 tool call 项填充子实例链接；子查询失败时记录日志并降级为无链接，不让 detail 请求失败
- [ ] 2.4 service 单测：嵌套 spawn 带链接、根派生省略 parent、失败/无匹配 spawn 无链接、子查询错误降级、非 spawn tool call 形状不变

## 3. Handler 与 API 契约

- [ ] 3.1 确认 handler 响应自然携带新字段（无逻辑变化），补充/更新 `internal/handler/subagent_test.go` 断言
- [ ] 3.2 更新 `api/openapi.yaml`：run 列表与详情响应 schema 增加 `parent_instance_id`，transcript tool call 项增加可选 `sub_agent_instance_id` / `sub_agent_name`，描述链接语义与省略条件

## 4. 集成测试与验收

- [ ] 4.1 在 `test/integration/dynamic_subagents_test.go`（或 harness fake）扩展两层嵌套场景：fake store 支持 `ListSubAgentChildRuns`，断言父 run 详情返回孙实例链接，且用该 id 能成功调用孙实例 runs/detail API
- [ ] 4.2 回归验证：根派生响应省略 parent 字段；`make test` 与 `go test ./test/integration/...` 全部通过
