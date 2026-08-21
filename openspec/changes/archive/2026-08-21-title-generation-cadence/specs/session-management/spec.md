# Delta: session-management

## MODIFIED Requirements

### Requirement: Auto generate session title
系统 SHALL 在 `POST /messages` 的 session-run claim 成功之后、orchestrator 启动之前,以 fire-and-forget goroutine 异步触发标题生成(与 turn 并行;被 409 `SESSION_BUSY` 拒绝的请求 SHALL NOT 触发)。触发 SHALL 按会话内用户消息序号 n(已持久化 user 行数 + 1,1 基)节流:n % 3 == 1(即第 1、4、7…条用户消息)时触发,其余消息 SHALL NOT 触发。生成输入 SHALL 为该会话首条用户消息与本次触发消息(不包含 assistant 内容)。标题写入 SHALL NOT 覆盖手动标题:`UpsertTitle` SHALL 以 SQL 原子守卫实现——已存在 `is_manual = TRUE` 的行 SHALL 保持原 `title`/`trace_id`/`is_manual` 不变(AI upsert 对其零效果);`is_manual = FALSE` 或不存在的行照常插入/覆盖。生成失败 SHALL 降级为本次触发消息前 20 字符并记警告日志。turn 结束后的持久化路径 SHALL NOT 再触发标题生成。

#### Scenario: 首条消息发送即触发
- **WHEN** 用户在会话中发送第 1 条消息(n=1),turn 尚未开始
- **THEN** 系统在 claim 成功后异步发起标题生成(不等待 turn 完成),生成不超过 20 字的标题写入 titles 表,`is_manual = FALSE`

#### Scenario: 每 3 条消息刷新一次
- **WHEN** 用户依次发送第 2、3、4、5 条消息
- **THEN** 仅第 4 条(n=4,n%3==1)触发标题生成;第 2、3、5 条不触发

#### Scenario: 生成输入为首条 + 本次消息
- **WHEN** 第 4 条消息触发标题生成
- **THEN** LLM 输入包含该会话首条用户消息与第 4 条消息,不包含任何 assistant 内容

#### Scenario: 生成失败降级
- **WHEN** 标题生成调用 OpenAI 失败
- **THEN** 系统使用本次触发消息前 20 字符作为默认标题,记录警告日志

#### Scenario: 会话忙时不触发
- **WHEN** 会话存在运行中 turn,新消息请求被 409 `SESSION_BUSY` 拒绝
- **THEN** 该请求不触发标题生成

#### Scenario: 触发时已是手动标题则早退
- **WHEN** 触发时 `GetTitle` 读到 `is_manual = TRUE` 的标题行
- **THEN** 系统跳过 LLM 调用,不发起生成

#### Scenario: 生成在 flight 中被手动改题不覆盖
- **WHEN** 标题生成已通过 manual 早退检查、LLM 调用进行中,用户此时通过 `PATCH /api/v1/sessions/:session_id` 设置了手动标题
- **THEN** 随后的 AI `UpsertTitle` 对该行零效果:手动标题、`trace_id` 与 `is_manual = TRUE` 全部保持

#### Scenario: 非手动行照常覆盖
- **WHEN** 已存在 `is_manual = FALSE` 的 AI 标题行,新一次生成完成
- **THEN** `UpsertTitle` 覆盖 `title`/`trace_id`,`is_manual` 保持 FALSE

#### Scenario: 中断或失败的 turn 仍有标题
- **WHEN** 触发消息对应的 turn 随后被取消或以错误结束
- **THEN** 标题生成不受影响(发送时已触发、detached context 下完成),turn 的中断路径不再额外触发标题
