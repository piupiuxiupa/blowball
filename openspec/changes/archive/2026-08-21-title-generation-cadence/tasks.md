# Tasks: title-generation-cadence

## 1. store 层(SQL 原子守卫)

- [x] 1.1 `internal/store/mysql/title.go`:`upsertTitleSQL` 的 `ON DUPLICATE KEY UPDATE` 改为 `title = IF(is_manual, title, VALUES(title)), trace_id = IF(is_manual, trace_id, VALUES(trace_id)), is_manual = is_manual`;注释更新(说明守卫语义:AI upsert 对 manual 行零效果)
- [x] 1.2 更新/核对 MySQL 侧与 service 层 fake store(`internal/service/title_test.go` 等)的 `UpsertTitle` 语义镜像,补"manual 行不被 AI upsert 改写"的回归用例

## 2. service 层(TitleService)

- [x] 2.1 `internal/service/title.go`:`GenerateTitle`/`generate`/`callLLM` 签名从 `(userMsg, assistantMsg)` 改为 `(firstUserMsg, currentUserMsg)`;prompt 组装改为双 User 段(首条提问 + 最新提问);sanitize 空回退取本次消息;manual 早退(`GetTitle` → `IsManual`)保留
- [x] 2.2 更新 `internal/service/title_test.go`:新签名的生成/降级/manual 早退用例;移除依赖 assistantMsg 的断言

## 3. handler 层(触发点与节流)

- [x] 3.1 `internal/handler/message_stream.go`:在 session-run claim 成功后、orchestrator 启动前新增触发点——计算 `n = prior 中 role==user 行数 + 1`,`n % 3 == 1` 时 `go h.titleSvc.GenerateTitle(...)`(首条 user 消息从 `prior` 取,n=1 时两参数同值);409 路径与 claim 失败降级路径的触发行为符合 spec(降级照常触发)
- [x] 3.2 删除旧触发路径:`persistEvents` 内 `isFirstTurn` 标题分支与 assistantContent 拼接循环(`message_stream.go:504-557` 区域);确认错误/取消路径的注释同步(不再有"fires first-turn title generation"语义)

## 4. 测试与收尾

- [x] 4.1 handler 单测:发送即触发(n=1)、n=2/3/5 不触发、n=4 触发、409 不触发、输入为首条+本次消息、turn 失败/取消后标题照常(发送时已触发)
- [x] 4.2 集成测试(含 fake MySQL 的 `UpsertTitle` 守卫语义):manual 在 flight 场景(早退检查通过后手动改题,AI upsert 零效果);既有 interrupted-turn 标题用例移除/改写为"发送时已触发"
- [x] 4.3 `make test`、`make lint` 通过;CLAUDE.md 中标题生成时机、`interrupted-turn-persistence` 相关描述、请求流程第 11 步同步更新;前端 repo 提示(turn 进行中标题会变,发送后 refetch 列表/详情)
