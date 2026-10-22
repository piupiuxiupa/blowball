## MODIFIED Requirements

### Requirement: 原始 payload 保真入库

`kind=request` 行的 `raw` MUST 为实际发送的完整请求 JSON(messages、tools、全部参数),在 params 组装完成后捕获,而非重建近似。唯一例外:`image_url` content part 中的 base64 数据 URL MUST 在捕获前替换为结构化脱敏占位 `{"type":"image_url_redacted","mime":...,"bytes":...,"sha256":...,"attachment_id":...}`,文本 part 与其余字段原样保留。`kind=response` 行的 `raw` MUST 为由流式分片缝合重构的完整响应等价 JSON,保留聚合结构(内容、tool_calls、finish_reason、usage、reasoning 及网关扩展字段)。`raw` 列 SHALL 为 MEDIUMTEXT;request/response/error 行系统 MUST NOT 压缩或截断(脱敏占位是字段级替换,不属于截断)。系统 MUST NOT 为新的流式帧产生 `kind=chunk` 行;既有 `kind=chunk` 行 SHALL 继续按其写入时的原始帧 payload 读取。

#### Scenario: 请求侧为实发全量
- **WHEN** 一次带工具与思维参数的调用被捕获
- **THEN** `kind=request` 的 `raw` 可解析出与实发 `ChatCompletionNewParams` 一致的 messages 数组、tools 数组与参数字段

#### Scenario: 图片 part 脱敏
- **WHEN** 一次调用的 user 消息携带 `image_url` base64 content part 被捕获
- **THEN** `kind=request` 的 `raw` 中该 part 为 `image_url_redacted` 结构(含 mime、原字节数、sha256、attachment_id),不包含任何 base64 数据;同消息的文本 part 与相邻字段字节原样

#### Scenario: 响应侧含被丢弃字段
- **WHEN** 网关在流中返回 usage 或扩展字段
- **THEN** `kind=response` 的 `raw` 含聚合后的该字段,不因现有 `LLMResponse` 结构体未承载而丢失
