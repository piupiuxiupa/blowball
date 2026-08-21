## ADDED Requirements

### Requirement: Message input limit

系统 SHALL 对 `POST /api/v1/sessions/:session_id/messages` 的用户输入设置两层入口防线，且全部校验 SHALL 在任何存储读写与该会话的 run 认领（见 turn-run-lifecycle）之前完成：

1. **请求体字节上限**：JSON 解析前请求体 SHALL 被限制在固定字节上限（1MB 常量，非配置项）内；超限请求 SHALL 返回 `413` 错误码 `REQUEST_TOO_LARGE`。
2. **输入 token 上限**：`content` 的估算 token 数超过 `messages.max_input_tokens` 时，请求 SHALL 被拒绝并返回 `400` 错误码 `CONTENT_TOO_LONG`，message SHALL 携带生效上限值与估算值。

token 数 SHALL 采用保守的字符类启发式估算（CJK 类 rune 每 1 个计 1 token，其余每 4 个字符计 1 token），估算结果 SHALL NOT 依赖网关侧精确计数。配置语义：`messages.max_input_tokens` 未配置时生效上限 SHALL 为 5000；显式配置 `0` SHALL 关闭 token 检测（字节上限不受影响）；负值 SHALL 在配置加载时被拒绝。校验拒绝的请求 SHALL NOT 认领该会话的 run 槽，也 SHALL NOT 触发任何 LLM 调用。

#### Scenario: Oversized request body

- **WHEN** 用户发送 POST /messages，请求体超过字节上限
- **THEN** 系统返回 HTTP 413，body 为 `{"error": {"code": "REQUEST_TOO_LARGE", ...}}`
- **AND** 该请求未读取任何存储、未认领该会话的 run 槽

#### Scenario: Content exceeds token limit

- **WHEN** 用户发送 POST /messages，`content` 的估算 token 数超过生效上限（默认 5000）
- **THEN** 系统返回 HTTP 400，body 为 `{"error": {"code": "CONTENT_TOO_LONG", "message": ...}}`，message 携带上限值与估算 token 数
- **AND** 该请求未认领该会话的 run 槽、未触发 LLM 调用

#### Scenario: Content within token limit passes

- **WHEN** `content` 估算 token 数不超过生效上限
- **THEN** 请求按既有流程继续（校验不改变正常输入的行为）

#### Scenario: Estimation is CJK-aware

- **WHEN** `content` 为纯 CJK 文本，长度等于生效上限字符数
- **THEN** 估算 token 数不低于该字符数（每个 CJK rune 至少计 1 token，估算方向保守偏高）

#### Scenario: Default limit applies when unset

- **WHEN** 配置未设置 `messages.max_input_tokens`
- **THEN** 生效上限为 5000（token 检测默认开启）

#### Scenario: Explicit zero disables token detection

- **WHEN** 配置显式设置 `messages.max_input_tokens: 0`
- **THEN** token 检测关闭（任意长度 `content` 不因 token 上限被拒），字节上限仍然生效

#### Scenario: Negative value rejected at load

- **WHEN** 配置设置 `messages.max_input_tokens` 为负数
- **THEN** 配置加载失败，服务拒绝启动
