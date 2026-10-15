## Context

主对话链路当前是纯文本管道：`agent.Message.Content` 为 string（93 个调用点），历史重建、compaction、消息持久化全部假设文本。`xizhi_read_file` 对二进制文件（含一切图片）直接拒绝。OpenAI Chat Completions 的 `role="tool"` 消息只接受文本 content（base64 塞入只会得到一坨字符文本，不进入视觉编码器），因此"把图作为工具结果回传主模型"在协议上不可行；而主模型（部署可选任意目录内模型）也未必具备视觉能力。

代码库已有完全同构的先例：webfetch 的 threshold-gated digest——工具内部通过窄接口 `PromptClient.Prompt(ctx, system, user, maxOutputTokens)` 调用配置指定的消化模型，拿回文本作为工具结果；`WebfetchDigestConfig` 的 `model` 在 config validate 阶段强制可解析到 `openai.models` 目录。本设计把该模式从"文本消化"平移到"图像消化"。

关键事实：图像作为**识图模型请求里 user message 的 `image_url` content part（base64 data URL）**是多模态输入的标准合法位置，与 tool-result 协议限制无关。图像因此可以完全不经过主模型协议与主对话持久化。

## Goals / Non-Goals

**Goals:**

- 任意主模型（含纯文本模型）获得定向识图能力：`xizhi_view_image(path, question)` → 指定识图模型 → 文本描述。
- 主链路零改动：`agent.Message`、消息持久化/历史重建、compaction、HTTP API、数据库 schema 全部不动；无 migration。
- 复用现有不变量：per-user workspace 路径沙箱、统一 tool-result envelope、tool 执行超时、llm-raw-capture 全量捕获。
- 部署可控：默认 `enabled: false`；识图模型必须能解析到模型目录，否则启动失败（对齐 webfetch digest model 校验）。

**Non-Goals:**

- 用户在聊天消息中直接附图、主模型原生多模态上下文（content parts、消息附件表、历史图重放与降级）——明确的后续二期方向。
- 服务端图片解码/缩放/重编码（token 优化留待二期；v1 原样 base64 透传，不引入 `x/image` 依赖）。
- 多图批量参数、`detail` low/high 档位、OCR 专用工具、`xizhi_read_file` 读到图片时自动转调识图。
- 识图结果缓存（未来可按 path+mtime+question 哈希缓存）。

## Decisions

### D1: 副驾识图（sidecar）而非主链路多模态

工具内部调用识图模型，主模型只看到文本描述。

**备选**：主模型原生多模态（`agent.Message` 扩展 content parts + `message_attachments` 表 + 历史重建注入图 + compaction 图降级）。否决于第一期：波及 93 个 `Message` 调用点、持久化/重建/摘要全链路、需要 migration；且图进历史会导致每轮 token 重放。副驾方案信息保真有损耗（二手转述），但以 `question` 定向识图弥补，且成本/风险低一个数量级。

### D2: 图像以 base64 data URL 内联发给识图模型

`{type:"image_url", image_url:{url:"data:<mime>;base64,<...>"}}` 作为识图模型 user message 的 content part。

**备选**：公网/签名 URL（要求网关能回访部署网络，私有部署基本不通）；Files API（OpenAI/Anthropic/Gemini 互不兼容，第三方 OpenAI 兼容网关普遍未实现）。base64 inline 兼容面最大，且与现有 `OpenAIClient` 单出站连接模型一致。

### D3: 新窄接口 `VisionClient`，生产实现落在 agent 包，复用 raw-capture 路径

```go
// internal/tool/xizhi 侧（形态对齐 webfetch.PromptClient）
type VisionClient interface {
    Describe(ctx context.Context, question string, image []byte, mime string) (string, error)
}
```

生产实现（`internal/agent`，类似 `webfetchPromptClient`）：构造带 image part 的 Chat Completion 请求调用指定 `model`。实现**必须**复用 `OpenAIClient` 的请求路径，使 llm-raw-capture 按"每次 LLM 调用全量捕获"语义免费覆盖识图调用，归属 `agent="vision"`（`agent.WithAgentName`，同 compaction 的 `"compaction"` 先例）。请求参数翻译（thinking 分支的 `max_completion_tokens`/`max_tokens` 等 wire family）必须复用现有目录解析逻辑，不得另起一套。

不扩展公共 `agent.Message`（不动历史重建）；也不在 `LLMRequest` 上加通用字段，而是给 client 增加独立的视觉调用入口，避免给主链路请求结构引入多模态歧义。

### D4: 配置块 `tools.xizhi.view_image`，model 强制目录解析

```yaml
tools:
  xizhi:
    view_image:
      enabled: true          # 默认 false
      model: glm-4v-flash    # 必须存在于 openai.models 目录
      max_image_bytes: 8388608  # 默认 8 MiB；零/负回退默认
      max_output_tokens: 2048
      timeout: 60s
```

`enabled: true` 但 `model` 无法在 `openai.models` 目录解析到时，config validate 启动失败（规则与 `tools.webfetch.digest.model` 一致）。`timeout` 同时可通过现有 `tools.timeouts` 的按工具名映射（`xizhi_view_image` 条目）生效，走既有 SetTimeouts 机制。

`max_image_bytes` 默认 8 MiB 的依据：base64 后约 10.7 MiB，加 JSON 包装仍低于 `llm_raw_log.raw` 的 MEDIUMTEXT 16 MiB 上限，保证 raw-capture "MUST NOT 截断"语义与落库不冲突。

### D5: 魔数嗅探 MIME，原样透传，不解码

按文件头魔数（而非扩展名）识别 `image/png`、`image/jpeg`、`image/webp`、`image/gif`；其余格式返回明确错误（提示支持列表）。v1 不做服务端解码/缩放/重编码——零新依赖（webp 解码本需 `x/image`），且避免重编码造成的画质/语义损失。文档中标注：如需 token 优化二期再做长边缩放。

### D6: `question` 可选但受工具描述强引导

缺省时使用通用描述提示（"详细描述图像内容，包括图中可见的文字"）。工具 description 明确引导模型传入定向问题（如"这张报错截图的错误堆栈是什么"）——定向问题的识图精度显著优于泛泛描述。

### D7: 结果 shape

```json
{"path":"shots/err.png","mime_type":"image/png","size":102400,"model":"glm-4v-flash","description":"..."}
```

经统一 envelope 回传：`{"status":0,"result":{...}}`；失败走 `{"status":1,"error":...}`（tool-result-envelope 对注册表工具自动生效）。`description` 长度受 `max_output_tokens` 约束（请求侧 `max_tokens`/`max_completion_tokens`）。

### D8: `reasoning_effort` 由 Agent 按调用自选

工具暴露可选 `reasoning_effort`（闭集 `none|low|medium|high|xhigh|max`，缺省归一化 `none`）。识图模型为 thinking 条目时按字面值下发（`none` 仍发送，保持"禁用思考"语义与全局 wire family 一致）；非 thinking 条目下非 `none` 值返回明确工具错误（对齐 per-request-model-selection 的 fail-fast 哲学：显式冲突报错、缺省宽容），错误信息引导 Agent 省略参数重试。闭集校验落在工具参数解析层（xizhi），能力校验落在模型边界（agent visionClient）——前者是 args 语法、后者依赖目录解析结果。工具描述引导：简单描述用缺省，密集图表/复杂 UI 截图/精细 OCR 才升档，避免默认烧推理 token。

## Risks / Trade-offs

- [转述损耗/识图模型幻觉] → `question` 定向识图 + 工具描述引导；结果携带 `model` 字段明示描述来源；对精度敏感场景引导模型分多问调用。
- [base64 全量进 llm_raw_log，表体积增长] → `max_image_bytes` 上限保证单行 < 16 MiB MEDIUMTEXT；表"永不自动清理"是既有语义，操作员可按需清理；识图调用以 `agent="vision"` 标注便于识别。
- [网关/识图模型不支持 image_url parts] → 默认 disabled；启用前应实测网关透传；调用失败按普通工具错误透传（模型可感知并告知用户）。
- [识图延迟（额外一次 LLM 往返）] → `timeout`（默认 60s）+ 现有按工具名超时映射；超时按工具错误返回。
- [同一图反复识图的费用] → v1 无缓存；工具描述引导模型复用已有描述；缓存列为二期。

## Migration Plan

无 migration、无 schema/API 变更。默认 `enabled: false`，存量部署行为不变。部署方显式启用并配置识图模型后生效；回滚 = 关闭开关。

## Open Questions

- GIF 动图语义（模型通常只看首帧）：v1 原样透传，由识图模型自行处理；是否抽帧待二期。
- `detail` low/high 档位是否暴露为工具参数（OpenAI 计价/tile 语义）：二期评估。
- 识图描述是否值得按 `(path, mtime, question)` 缓存：待真实使用频率验证。
