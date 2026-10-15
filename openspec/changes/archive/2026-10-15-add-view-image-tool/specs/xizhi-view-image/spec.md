## ADDED Requirements

### Requirement: Xizhi view image tool
系统 SHALL 提供 workspace 作用域的识图工具 `xizhi_view_image`，接受参数 `path`（workspace 相对路径）与可选 `question`（定向识图问题），读取请求用户 workspace 内的图片文件并返回文本描述。工具 SHALL 复用 xizhi 既有的 per-user 路径沙箱校验：绝对路径、`..`、符号链接逃逸与 workspace 之外的路径 MUST 被拒绝。

#### Scenario: 带定向问题识图
- **WHEN** Agent 调用 `xizhi_view_image`，path 为 `"shots/error.png"`，question 为 `"这张截图里的报错堆栈是什么"`
- **THEN** 系统读取 `data/{user_uuid}/workspace/shots/error.png`，调用配置的识图模型，返回包含 path、mime_type、size、model 与 description 的结果对象

#### Scenario: 缺省 question 使用通用描述提示
- **WHEN** Agent 调用 `xizhi_view_image` 只提供 path，未提供 question
- **THEN** 系统使用通用描述提示（详细描述图像内容，包括图中可见的文字）调用识图模型

#### Scenario: 读取 workspace 之外的路径被拒绝
- **WHEN** Agent 调用 `xizhi_view_image`，path 为 `"../../etc/passwd"`
- **THEN** 系统拒绝操作并返回错误 "path outside workspace"

#### Scenario: 文件不存在
- **WHEN** Agent 调用 `xizhi_view_image`，path 指向的文件不存在
- **THEN** 系统返回错误 "file not found"

### Requirement: 支持格式与大小约束
工具 SHALL 按文件头魔数（而非扩展名）识别图片 MIME 类型，且仅接受 `image/png`、`image/jpeg`、`image/webp`、`image/gif`。不支持的格式 SHALL 返回列出支持格式的明确错误。文件大小超过 `tools.xizhi.view_image.max_image_bytes` SHALL 返回明确的大小超限错误；`max_image_bytes` 未配置、为零或负值时 SHALL 回退默认值 8388608（8 MiB）。

#### Scenario: 支持的图片格式被接受
- **WHEN** path 指向 PNG/JPEG/WebP/GIF 图片文件
- **THEN** 系统按魔数识别出对应 mime_type 并继续识图调用

#### Scenario: 不支持的格式被拒绝
- **WHEN** path 指向扩展名为 `.png` 但内容为 PDF 的文件
- **THEN** 系统按魔数判定非图片，返回包含支持格式列表的错误

#### Scenario: 图片超过大小上限
- **WHEN** 图片文件字节数超过 `max_image_bytes`
- **THEN** 系统返回大小超限错误，且不发起识图模型调用

#### Scenario: 大小上限默认值
- **WHEN** `max_image_bytes` 未配置，或配置为 0 或负值
- **THEN** 系统使用默认上限 8388608 字节

### Requirement: 指定识图模型的 base64 内联调用
识图调用 SHALL 作为一次独立的 Chat Completion 请求发给 `tools.xizhi.view_image.model` 指定的识图模型：图片以 base64 data URL 编码为 user message 的 `image_url` content part，question 作为同消息的 text part。图像 SHALL NOT 进入主对话的请求、历史或持久化。`tools.xizhi.view_image.enabled` 为 true 时，`model` MUST 能解析到 `openai.models` 目录中的条目，否则配置加载 MUST 报错并拒绝启动。`max_output_tokens`（默认 2048，零/负回退）SHALL 作为该请求的输出 token 上限。识图模型调用失败或超时 SHALL 作为普通工具错误返回，主对话流程 MUST NOT 因此中断。

#### Scenario: 识图模型收到图与问题
- **WHEN** 一次合法的 `xizhi_view_image` 调用执行
- **THEN** 系统向配置的识图模型发出一次请求，user message 由 question 文本 part 与 `image_url` base64 data URL part 组成
- **AND** 主模型收到的工具结果中不包含图像数据

#### Scenario: 识图模型不在模型目录
- **WHEN** 配置 `tools.xizhi.view_image.enabled: true` 且 `model` 不在 `openai.models` 目录中
- **THEN** 配置加载失败，服务拒绝启动并报出明确错误

#### Scenario: 识图模型调用失败
- **WHEN** 识图模型请求返回错误或超时
- **THEN** 工具按错误 envelope（`{"status":1,"error":...}`）返回，当前 turn 继续执行

### Requirement: Agent 自选识图思考等级
`xizhi_view_image` SHALL 提供可选参数 `reasoning_effort`，取值为闭集 `none|low|medium|high|xhigh|max`，缺省或空 SHALL 归一化为 `none`；由调用方 Agent 按图片复杂度自行选择。当识图模型为 thinking 条目时，请求 SHALL 携带该 effort 字面值（含 `none`）；当识图模型为非 thinking 条目时，`none` SHALL 正常执行（不发 reasoning_effort），非 `none` 值 SHALL 返回明确的工具错误（提示该部署识图模型不支持推理，应省略参数或改用 `none`），非法枚举值 SHALL 在参数解析层被拒绝。

#### Scenario: Agent 为复杂图表请求高思考等级
- **WHEN** Agent 调用 `xizhi_view_image`，reasoning_effort 为 `"high"`，且识图模型为 thinking 条目
- **THEN** 识图请求携带 `reasoning_effort: "high"` 与 `max_completion_tokens`（thinking wire family）

#### Scenario: 缺省 effort 归一化为 none
- **WHEN** Agent 调用 `xizhi_view_image` 未提供 reasoning_effort，且识图模型为 thinking 条目
- **THEN** 识图请求携带字面值 `reasoning_effort: "none"`（禁用思考的既有副驾行为不变）

#### Scenario: 非思考识图模型拒绝非 none effort
- **WHEN** 识图模型为非 thinking 条目，Agent 传 reasoning_effort 为 `"high"`
- **THEN** 工具返回错误 envelope，错误信息指明该识图模型不支持 reasoning 并建议省略参数或使用 `none`

#### Scenario: 非法枚举值被拒绝
- **WHEN** Agent 传 reasoning_effort 为 `"extreme"`（不在闭集内）
- **THEN** 参数解析层返回错误，且不发起识图模型调用

### Requirement: 配置开关与注册
`tools.xizhi.view_image.enabled` SHALL 控制工具注册，默认为 false；未启用时 `xizhi_view_image` MUST NOT 出现在工具注册表与任何 Agent 的可用工具中。启用后，工具 SHALL 遵循现有 agent 配置的 tools 列表机制：仅列出 `xizhi_view_image` 的 agent 可调用它。工具 SHALL 纳入现有按工具名的执行超时机制（`tools.timeouts` 的 `xizhi_view_image` 条目与配置块 `timeout`，默认 60s，零/负回退）。

#### Scenario: 未启用时不注册
- **WHEN** `tools.xizhi.view_image.enabled` 为 false 或未配置
- **THEN** 工具注册表中不存在 `xizhi_view_image`

#### Scenario: 启用后注册
- **WHEN** `tools.xizhi.view_image.enabled` 为 true 且 model 可解析
- **THEN** `xizhi_view_image` 注册进工具注册表，可被 agent 配置的 tools 列表引用

#### Scenario: 默认关闭不影响存量部署
- **WHEN** 部署升级后未修改配置
- **THEN** 行为与升级前一致，无识图模型调用发生

### Requirement: 识图调用的 raw capture 归属
识图模型调用 SHALL 经由与主对话相同的 LLM 客户端请求路径发出，从而被 llm-raw-capture 按"每次 LLM 调用全量捕获"既有语义捕获；捕获行的 `agent` 列 SHALL 为 `"vision"`，并携带与触发 turn 一致的 trace/session/user 关联列。

#### Scenario: 识图调用被捕获并归属 vision
- **WHEN** 一次 `xizhi_view_image` 调用触发识图模型请求
- **THEN** `llm_raw_log` 落库该调用的 request 与 response/error 行，`agent` 列为 `"vision"`，trace_id/session_id/user_id 与触发它的 turn 一致
