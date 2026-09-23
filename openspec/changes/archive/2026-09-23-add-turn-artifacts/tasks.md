# Tasks: add-turn-artifacts

## 1. 存储与配置

- [x] 1.1 新增 migration：`file_versions` 表（`id` PK、`user_id`、`path`、`version_id`(uuidv7)、`size`、`mime`、`sha256`、`created_at`；唯一键 `(user_id, path, version_id)`，索引 `(user_id, path, created_at)`）
- [x] 1.2 新增配置项：`artifact.version_store_root`（默认 `<dataDir>/versions`）、`artifact.max_snapshot_bytes`（默认 200MB）；同步 `config.example.yaml` 与 `internal/config` 校验
- [x] 1.3 新增 `internal/artifact` 包：`Store`（blob 写入 `<root>/<user>/<url-escape(path)>/<vid>`、sha256 去重复用 vid、按 vid 读取）、`Index`（MySQL 索引增查：insert、latest-by-path、as-of resolve、owner 校验）

## 2. 产物检测与版本快照

- [x] 2.1 `internal/artifact` 实现 `Detect(wsRoot string, since time.Time) []string`：遍历工作空间，收集 `mtime >= since` 的常规文件，排除 `tmp/`、`.blowball/`、隐藏目录；单测覆盖排除规则
- [x] 2.2 实现 `Finalize(ctx, userID, wsRoot, paths)`：对每个产物 stat（size/mime 推断）→ 快照写入版本库 → 索引记录 → 返回 `[]Artifact{Path, VersionID, Size, Mime, Op}`；超上限文件跳过快照仅记录 path；单测覆盖去重复用 vid

## 3. 流事件与编排接入

- [x] 3.1 `internal/stream/event.go`：新增 `EventArtifact = "artifact"` 与 `ArtifactEvent(agent string, a ArtifactPayload)` 构造函数（content 为 JSON：path/version_id/size/mime/op）
- [x] 3.2 `TurnHooks`（`internal/handler/ports.go`）新增 `FinalizeTurn(ctx) []stream.StreamEvent`；orchestrator 在发 done 前调用，逐个发出返回的 artifact 事件，并把摘要数组写入 done meta 的 `artifacts` 键
- [x] 3.3 `message_stream.go` turn 启动时记录 `turnStart`；turn 结束时（经由 3.2 的钩子）调 `Detect` + `Finalize` 产出事件
- [x] 3.4 `message_reconstruct.go`：`MessagesToAgentMessages` 跳过 `artifact` 事件类型；单测钉住该行为

## 4. HTTP 接口

- [x] 4.1 `GET /api/v1/workspace/versions/resolve?path=&before=`：最新/as-of 版本解析，403 越界、404 无记录
- [x] 4.2 `GET /api/v1/workspace/versions/{versionId}/content`：Bearer 或 `?token=` 鉴权（复用 QueryTokenAuthMW 模式），属主校验失败按 404 处理
- [x] 4.3 `GET /api/v1/workspace/versions/{versionId}/onlyoffice-config`：签名 view 配置（document.url 指向 4.2、key 复用 base32(sha256) 派生、无 callbackUrl）；未配置 secret 返回 503
- [x] 4.4 路由注册接入 `router.go` workspace 文件路由族；为 4.1–4.3 补 handler 单测与集成测试
- [x] 4.5 更新 `api/openapi.yaml`：三个新接口 + `artifact` 事件类型说明 + done meta `artifacts` 字段

## 5. Prompt 约定

- [x] 5.1 `internal/prompt/render.go` 的 `renderWorkspaceConvention()` 增加产物链接语法约定（`[显示名](blowball://workspace/<相对路径>)`、仅限交付物、不含查询参数、禁止 tmp/）
- [x] 5.2 更新 `internal/prompt/render_test.go`：`TestRenderSystemPrompt_FileOutputConvention` 增加链接约定断言

## 6. 交付与验证

- [x] 6.1 `make test`（含 race）与 `make lint` 全绿
- [x] 6.2 端到端冒烟：一个 turn 产出 docx → SSE 出现 artifact 事件 → done 摘要正确 → 历史重放还原产物 → 版本内容/预览配置接口可用 → 覆盖后旧 vid 仍可读旧内容（需真实 MySQL/Redis/服务进程，当前沙箱无法绑定端口，留待部署环境验证）

## 7. 版本后端切换到 office-vers（评审后修订）

- [x] 7.1 删除本地 BlobStore，新增 `VersionStore` 接口 + office-vers HTTP client（`POST /documents/{uid}/{path}` 取 versionId，`GET ?action=version&versionId=` 读回）
- [x] 7.2 删除 `artifact.version_store_root` 配置与 serve.go 的 versionsDir；版本功能开关并入 `onlyoffice.version_service_url`（空则降级为无 vid 的产物事件）
- [x] 7.3 删除自建的 `GET /versions/:vid/onlyoffice-config`（office 历史版本预览复用既有 `/files/*path/onlyoffice-version-config`）；openapi.yaml 与 frontend-handoff.md 同步
- [x] 7.4 spec/design 同步修订（快照写入 office-vers、未配置降级、删除自建预览配置端点）
