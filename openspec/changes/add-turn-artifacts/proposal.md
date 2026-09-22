# Proposal: add-turn-artifacts

## Why

Agent 最终回复中的文件产物目前只是纯文本路径，前端无法识别为可点击元素，用户要手动去工作区翻找。同时产物文件会被后续 turn 覆盖，历史消息中提到的文件打开后已不是当时的内容。行业内（ChatGPT sandbox 链接、Manus 文件面板、Claude Artifacts）均已把"产物可点击、可回溯"作为标准能力。

## What Changes

采用"prompt 链接约定（A）+ 结构化 artifact 事件（C）"的混合方案：

- **链接约定**：系统提示词新增约定——agent 在最终回复中引用交付物时必须使用 `[文件名](blowball://workspace/<相对路径>)` 形式的 markdown 链接；链接永不指向 `tmp/`。
- **产物检测与版本快照（turn 末）**：每个 turn 结束时，后端扫描工作区（mtime ≥ turn 开始时间，排除 `tmp/`、`.blowball/` 等保留目录），对每个本轮产物做内容快照存入服务端版本库，生成时间有序的 `version_id`。
- **artifact 流事件**：turn 末为每个产物发出一个 `artifact` 流事件（path、version_id、size、mime、op），随现有事件流持久化；`done` 事件 meta 附带本轮产物摘要（仅实时用，不持久化）。
- **版本服务接口**：新增按 version_id 读取版本内容的接口，以及 OnlyOffice 历史版本只读预览配置接口（document.url 指向自有版本接口，复用现有签名与 key 派生模式）。
- **旧链接指向旧版本**：消息文本中的链接不改写、不落 vid；前端按"消息所属 turn 的 artifact 事件"解析出 vid 进行钉版。后端另提供按路径+时间解析"当时版本"的兜底接口，覆盖跨 turn 引用。
- **office 文件默认预览**：前端点击 office 产物默认使用现有 config 接口返回的 View 配置打开（历史版本走新版本预览接口）；后端无需改动编辑逻辑。

## Capabilities

### New Capabilities
- `turn-artifacts`: turn 级产物检测、版本快照存储、artifact 流事件、版本内容读取与版本预览配置接口、链接解析契约。

### Modified Capabilities
- `system-prompt-rendering`: "Workspace file output convention" 要求扩展——新增交付物 markdown 链接语法约定（blowball:// scheme）及"链接永不指向 tmp/"规则。

## Impact

- **流协议**：新增 `artifact` 事件类型（`internal/stream`）；`done` 事件 meta 新增 `artifacts` 摘要字段。事件持久化沿用现有管道，需确认消息重建（`message_reconstruct`）对 artifact 事件的处理（折叠为上下文提示或跳过）。
- **编排层**：`internal/agent/orchestrator.go` 在发出 done 前调用新增的产物收尾钩子；`internal/handler/ports.go` TurnHooks 扩展。
- **新增包**：`internal/artifact`（检测、快照、版本库读写、mime 推断）。
- **HTTP**：新增版本内容/版本预览配置/版本解析接口，路由挂在 workspace 文件路由族下；更新 `api/openapi.yaml`。
- **Prompt**：`internal/prompt/render.go` 文件输出约定段落扩展。
- **存储**：服务端版本库目录（workspace 之外，默认 `<dataDir>/versions/<user>/`）+ MySQL 版本索引表（新 migration）。
- **前端**：链接渲染拦截、产物面板、点击动作路由——不在本仓库实现，交接文档见 `frontend-handoff.md`。
- **部署**：无需新服务；office-vers 外部服务保持现状（其版本仍只覆盖 OnlyOffice 编辑路径）。
