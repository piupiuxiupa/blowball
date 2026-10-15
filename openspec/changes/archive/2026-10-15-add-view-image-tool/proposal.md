## Why

Agent 目前完全无法理解图像：用户上传到 workspace 的截图/图表是二进制文件，`xizhi_read_file` 按二进制嗅探直接拒绝读取；而 OpenAI Chat Completions 的 `role="tool"` 消息只接受文本 content，图像无法作为工具结果回传主模型。主模型也未必具备视觉能力。需要一个"视觉副驾"工具：工具内部调用部署方指定的识图模型，把图像转化为文本描述返回，让任意（含纯文本）主模型获得定向识图能力，且不改动主对话链路。

## What Changes

- 新增 workspace 作用域工具 `xizhi_view_image`：参数 `path`（workspace 相对路径）+ `question`（定向识图问题），读取用户 workspace 中的图片文件。
- 工具内部将图片以 base64 data URL 作为识图模型 user message 的 `image_url` content part 发起一次独立的 LLM 调用（这是多模态输入的标准合法位置，绕开 tool result 不能带图的协议限制），取回文本描述。
- 工具结果为纯文本描述，经统一的 tool-result envelope（`{"status":0,"result":...}`）回传主模型。
- 新增 `tools.xizhi.view_image` 配置块：`enabled`、`model`（识图模型，MUST 可解析到 `openai.models` 目录，校验规则对齐 webfetch digest model）、`max_image_bytes`、`max_output_tokens`、`timeout`。
- 复用现有机制：xizhi per-user 路径沙箱校验、tool 执行超时、llm-raw-capture（识图调用经 OpenAI client 发出，以 `agent="vision"` 归属自然被捕获）。
- 明确不改动：`agent.Message` 结构、消息持久化/历史重建/compaction、`sendMessageRequest` API、数据库 schema——图像永不进入主对话上下文，第一期无 migration、无 openapi.yaml 变更。

## Capabilities

### New Capabilities
- `xizhi-view-image`: workspace 图片的定向识图工具——路径沙箱、MIME/大小约束、指定识图模型的 base64 内联调用、文本描述回传与配置开关。

### Modified Capabilities

（无。tool-result-envelope、tool-execution-timeout 对新注册工具自动生效；llm-raw-capture 的"每次 LLM 调用全量捕获"要求已覆盖新增调用方，均无 spec 级行为变更。）

## Impact

- **代码**：`internal/tool/xizhi/`（新工具注册与实现，`RegisterAll` 纳入）、`internal/agent/openai_client.go`（暴露或新增窄接口 `VisionClient`，形态对齐 webfetch 的 `PromptClient`）、`internal/config/`（新配置块 + 校验 + 默认值）、`cmd/blowball/serve.go`（接线）、`config.example.yaml`。
- **依赖**：Go 标准库可解码/缩放 png、jpeg；webp/gif 解码如需支持引入 `golang.org/x/image`（或首期限定 png/jpeg/webp，按网关兼容性决定）。
- **运行影响**：每次调用产生一次额外识图模型 LLM 往返（延迟与费用）；识图请求的 base64 会按现行 llm-raw-capture 语义全量入库（MEDIUMTEXT 16MiB 上限内，`max_image_bytes` 默认值须保证 base64 后不超限）。
- **兼容性**：默认 `enabled: false`，不影响现有部署；主链路（消息 API、持久化、重建、摘要）零改动。
