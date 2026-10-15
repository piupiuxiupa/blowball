## 1. 配置与校验

- [x] 1.1 在 `internal/config` 新增 `ViewImageConfig`（enabled/model/max_image_bytes/max_output_tokens/timeout）与 `Resolve` 默认值回退（8MiB/2048/60s），挂到 `XizhiConfig.ViewImage`
- [x] 1.2 config validate：`enabled: true` 时 `model` 必须可经 `FindModelCatalogEntry` 解析，否则报错拒绝启动（对齐 webfetch digest model 校验）；补配置校验单测
- [x] 1.3 `config.example.yaml` 增加 `tools.xizhi.view_image` 示例块（默认注释关闭），说明识图模型需在 `openai.models` 目录中

## 2. 识图客户端

- [x] 2.1 在 `internal/agent` 实现生产 `VisionClient`（形态对齐 `webfetchPromptClient`）：构造 question text part + base64 data URL `image_url` part 的 user message，调用指定 model；复用 `OpenAIClient` 请求路径与 wire-family 参数翻译（thinking 分支 max_completion_tokens/max_tokens），设置 `max_output_tokens` 输出上限
- [x] 2.2 识图调用以 `agent.WithAgentName(ctx, "vision")` 归属，确认 llm-raw-capture 捕获 request/response 行且 `agent="vision"`、trace/session/user 关联正确
- [x] 2.3 单测：请求体含 image_url data URL part 与 question part、model/max_tokens 正确、错误与超时按 error 返回

## 3. 工具实现

- [x] 3.1 在 `internal/tool/xizhi` 新增 `VisionClient` 窄接口与 `view_image.go`：复用 `validatePath` 沙箱校验；魔数嗅探 MIME（png/jpeg/webp/gif），不支持格式与超 `max_image_bytes` 返回明确错误且不发起模型调用
- [x] 3.2 实现结果对象 `{path, mime_type, size, model, description}`；缺省 question 使用通用描述提示；question 透传给识图模型
- [x] 3.3 工具描述：写明用途、path 相对 workspace、question 定向引导（报错截图/图表/UI 走查示例）、支持格式与大小上限、结果是模型转述
- [x] 3.4 `RegisterAll` 按 `cfg.ViewImage.Enabled` 条件注册 `xizhi_view_image`（默认不注册），接入 `tools.timeouts` 按名超时
- [x] 3.5 `cmd/blowball/serve.go` 接线：构造生产 `VisionClient` 并传入 xizhi 注册（api 角色不启用）
- [x] 3.6 单测：沙箱拒绝/文件不存在/魔数不符/超限/envelope 成功与失败路径、注册开关行为

## 4. 端到端验证与收尾

- [x] 4.1 集成测试：启用配置 + fake/本地识图模型端点，走一轮带 `xizhi_view_image` 的 turn，验证工具结果 envelope、主对话不受识图失败影响、llm_raw_log 归属 vision
- [x] 4.2 `make test`（含 race）与 `make lint` 通过；`gofmt` 无 diff
- [x] 4.3 检查 `api/openapi.yaml` 无需变更（无新 HTTP 端点/契约变化）并确认；更新 README/config 文档中关于识图工具的说明（如有运维文档）
