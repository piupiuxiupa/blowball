# Tasks: add-input-token-limit

## 1. 配置层

- [x] 1.1 `internal/config/config.go`：`MessagesConfig` 增加 `MaxInputTokens *int`（yaml `max_input_tokens`），加载校验 —— 负数报错；归一导出带符号生效值（未配置→5000，显式 0→0=关闭），并补充 `config.example.yaml` 的 `messages:` 块注释示例
- [x] 1.2 `internal/config` 单测：未配置默认 5000；显式 0 关闭；正整数生效；负数加载失败

## 2. 估算助手

- [x] 2.1 新建 `internal/handler/tokens.go`：`estimateTokens(s string) int`（CJK 类 rune 计 1 token/个，其余 4 字符/token；`isCJK` 覆盖 Han/Hiragana/Katakana/Hangul 等区块）
- [x] 2.2 `internal/handler/tokens_test.go`：纯 CJK、纯 ASCII、混合、空串的数值断言（保守偏高方向）

## 3. Handler 校验链

- [x] 3.1 `internal/handler/message_stream.go`：包内常量 `maxMessageBodyBytes = 1 << 20`；`SendMessage` 在 `ShouldBindJSON` 前以 `http.MaxBytesReader` 包住 `c.Request.Body`，bind 出错先 `errors.As` 检测 `*http.MaxBytesError` → 413 `REQUEST_TOO_LARGE`（其余 bind 错误维持 400 BAD_REQUEST）
- [x] 3.2 `SendMessage` 在 `content` 非空检查之后、model/effort 解析之前插入 token 检查：`estimateTokens(req.Content) > 生效上限` → 400 `CONTENT_TOO_LONG`，message 形如 `content exceeds the 5000-token limit (estimated N tokens)`；上限 `<= 0`（显式关闭）时跳过检查
- [x] 3.3 把归一后的生效上限接入 `MessageStreamHandler`（构造参数或 config 字段，与现有 `ModelSelectionConfig` 注入风格一致）

## 4. 测试

- [x] 4.1 `internal/handler` 单测：超 1MB body → 413 `REQUEST_TOO_LARGE` 且未触达任何 store；估算超限 content → 400 `CONTENT_TOO_LONG`（message 含上限与估算值）；限内 content 正常走既有流程；上限 0 时超长 content 放行；拒绝路径未认领 run 槽（无 SESSION_BUSY 交互）
- [x] 4.2 集成侧（`test/integration/` 或 handler 级 wiring 测试）：配置默认值端到端生效 —— 未配置时 5000 上限拒绝超大输入

## 5. 契约与文档

- [x] 5.1 `api/openapi.yaml`：`/api/v1/sessions/{session_id}/messages` 的 POST 补充 400 `CONTENT_TOO_LONG` 与 413 `REQUEST_TOO_LARGE` 响应描述；同步 `blowball-frontend` 重新生成（`npm run generate-api`）
- [x] 5.2 更新 `CLAUDE.md`：端点表 messages 行补校验说明；Important conventions 的 `messages:` 块说明加 `max_input_tokens`
- [x] 5.3 `make test` + `make lint` 全绿
