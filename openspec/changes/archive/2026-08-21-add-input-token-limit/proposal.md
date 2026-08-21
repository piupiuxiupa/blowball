# Proposal: add-input-token-limit

## Why

`POST /api/v1/sessions/:session_id/messages` 对用户输入 `content` 目前只有非空校验，且该端点没有请求体字节上限（`MaxBytesReader` 只挂在文件上传上）。恶意或误操作的超大输入会一路通过 JSON 解析、历史恢复、run 认领，直到发往 LLM 网关才被拒（或者先在服务端缓冲超大 body 打爆内存）。需要在入口处设置两道防线：字节上限（DoS 后备层）与 token 估算上限（语义层）。

## What Changes

- `POST /messages` 入口新增两层校验，全部在 run claim（Redis SET NX）之前：
  1. **请求体字节上限**：bind 前对 `c.Request.Body` 套 `http.MaxBytesReader`（常量，~1MB）；超限 → `413 REQUEST_TOO_LARGE`（对齐 upload 端点的 `*http.MaxBytesError` 检测模式）。
  2. **输入 token 上限**：CJK 感知启发式估算 `content` token 数，超过上限 → `400 CONTENT_TOO_LONG`，message 携带上限值与估算值。
- 新配置 `messages.max_input_tokens`：默认 `5000`，`0` = 关闭检测，负数在 config 加载时报错（对齐 `messages:` 块现有键的校验惯例）。
- 估算算法：汉字类 rune 按 1 token/字、其余按 4 字符/token，宁可高估（保守拒绝）——防滥用红线下估算精度不追求，不引入 tokenizer 依赖。
- 范围仅此一个端点：`PATCH` 标题不加限制（已明确排除），文本写文件已有 413 字节上限兜底。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `session-management`: 新增 "Message input limit" 需求 —— 字节上限与 token 估算上限的两层入口校验、校验先于 run 认领的次序约束、`messages.max_input_tokens` 配置语义（默认/关闭/负数拒绝）、错误码与报错形状。

## Impact

- `internal/config/config.go` — `MessagesConfig` 增加 `MaxInputTokens` 字段与加载校验（负数拒绝；缺省 5000）。
- `internal/handler/message_stream.go` — `SendMessage` 入口套 `MaxBytesReader`、`*http.MaxBytesError` → 413；`content` 非空检查后插入 token 估算检查 → 400。
- `internal/handler/`（新文件 `tokens.go`）— CJK 感知 `estimateTokens` 助手。
- `config.example.yaml` — `messages:` 块注释示例。
- `api/openapi.yaml` — messages 端点补充 400 `CONTENT_TOO_LONG` / 413 `REQUEST_TOO_LARGE` 响应；前端重新生成类型。
- 无数据库、无路由、无 agent 层变更。
