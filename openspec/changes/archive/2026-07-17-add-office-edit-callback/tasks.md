## 1. 后端配置

- [x] 1.1 `internal/config/config.go` 新增 `OnlyOffice{ Secret, ServerURL, InternalBackend string }` 配置段，支持 `${VAR}` 展开；`ServerURL` 默认 `http://localhost`
- [x] 1.2 `config.example.yaml` 增加 `onlyoffice:` 段示例（secret / server_url / internal_backend），注明 secret 须与 DocumentServer `local.json` 一致

## 2. 后端"签发编辑器配置"端点

- [x] 2.1 `internal/handler/workspace.go` 新增 `OnlyOfficeConfig`：从 catch-all 读 `path`、`xizhi.ValidatePath` 校验（越界 403）、文件不存在 404
- [x] 2.2 构造 config：`documentType` 按扩展名；`document{fileType, key=随机, title, url, permissions{edit:true,download:true}}`；`editorConfig{mode:"edit", callbackUrl, customization{forcesave:true}, user}`
- [x] 2.3 `document.url`/`callbackUrl` 用 `onlyoffice.internal_backend` 拼 host，并附带请求的 Bearer 用户 JWT 作为 `token` query
- [x] 2.4 用 `onlyoffice.secret` 对 config 做 HS256 签名得 `token`；返回 `{server_url, config, token}`
- [x] 2.5 `router.go` 在 catch-all 分发加 `/onlyoffice-config` 后缀分支（Bearer 鉴权，仿 `/content`），`RouteDeps` 增 `WorkspaceOnlyOfficeConfig`

## 3. 后端"保存回调"端点

- [x] 3.1 `internal/handler/workspace.go` 新增 `OnlyOfficeCallback`：从 query 读 `path`、`xizhi.ValidatePath` 校验（越界 403），query-token 鉴权由中间件完成
- [x] 3.2 解析 JSON body `{status, key, url}`；`status` 为 1/3/4/7 时不写盘、返回 `{"error":0}`
- [x] 3.3 `status` 为 2/6 且 `url` 非空：`url` host 不等于 `onlyoffice.server_url` host 则拒绝、`{"error":1}` + 告警（SSRF）
- [x] 3.4 下载 `url`（`http.Client` 带超时），先写同目录临时文件、`os.Rename` 原子覆盖；超 `maxUploadBytes` 拒绝；失败 `{"error":1}`、原文件不动；成功 `{"error":0}`
- [x] 3.5 `router.go` 注册独立 `POST /api/v1/workspace/onlyoffice-callback`，挂 `QueryTokenAuthMiddleware`；`RouteDeps` 增 `WorkspaceOnlyOfficeCallback`

## 4. 接线

- [x] 4.1 `cmd/blowball/serve.go` 读取 `onlyoffice` 配置，构造 `WorkspaceHandler`（注入 secret/server_url/internal_backend），把两个 handler 注入 `RouteDeps`
- [x] 4.2 `onlyoffice` 配置缺失/secret 为空时，两个端点返回 503 并记录（避免用空 secret 签出无效配置）

## 5. 后端测试

- [x] 5.1 `internal/handler/workspace_test.go`：配置端点——正常返回签名配置、路径越界 403、文件不存在 404、未鉴权 401、`token` 可被同一 secret 验签
- [x] 5.2 回调端点——status=2/6 落盘、status=1/4 不写盘、host 不符拒绝、下载失败原文件不变、超限拒绝、缺 token 401、路径越界 403

## 6. 前端重构（去硬编码 + 去浏览器签名）

- [x] 6.1 `src/lib/onlyoffice.ts`：删除 `ONLYOFFICE` 常量、`signOnlyOfficeToken`、`buildOnlyOfficeConfig`；新增 `fetchOfficeConfig(path)` 调 `GET /api/v1/workspace/files/<path>/onlyoffice-config` 返回 `{server_url, config, token}`；保留 `loadOnlyOfficeApi(server_url)`（参数化脚本地址）
- [x] 6.2 `src/hooks/use-file-content.ts` 或新 hook：封装取配置的 TanStack Query
- [x] 6.3 `src/components/files/office-viewer.tsx`：打开文件 → 取配置 → 用 `server_url` 加载 api.js → `new DocEditor(id, {...config, token})`；保留 keyed remount 与刷新按钮（刷新=重取配置拿新随机 key）
- [x] 6.4 删除 `src/onlyoffice.d.ts` 中不再需要的部分（若有）；保留 `window.DocsAPI` 类型
- [x] 6.5 删除调试用 `public/oo-test.html`

## 7. 文档与端到端验证

- [x] 7.1 `api/openapi.yaml` 增加 `GET .../onlyoffice-config` 与 `POST .../onlyoffice-callback` 两个端点（并 `npm run generate-api` 重生成 `openapi.d.ts`）
- [x] 7.2 端到端：配置 `onlyoffice` 段 → 打开 office 文件进入编辑态 → 编辑 → forcesave/关闭 → 确认工作区原文件被覆盖 → 重新打开看到最新内容（新随机 key 触发重转）
  - **状态：待人工验证。** 需要运行中的 OnlyOffice DocumentServer + 完整后端栈，无法在当前环境执行。单元/集成测试已覆盖：配置签名（同 secret 验签 + payload==config）、回调落盘（status 2/6 原子覆盖）、不写盘（1/4）、SSRF host 拒绝、下载失败原文件不变、超限拒绝、缺 token 401、越界 403。人工 e2e 步骤见下方。
- [x] 7.3 `npm run lint && npm run build` 通过；`make test` 通过

### 7.2 人工端到端验证步骤（执行后勾选上方 7.2）
1. `docker compose up -d` 起 MySQL/Redis；启动 OnlyOffice DocumentServer 容器，其 `local.json` 的 `token.secret` 设为与下方 `onlyoffice.secret` 相同的值，并开启 `token.enable.browser/inbox/outbox`。
2. `config.yaml` 填 `onlyoffice: { secret: <同上>, server_url: <浏览器加载 api.js 的地址>, internal_backend: <容器可达后端地址> }`。
3. `make build && ./bin/blowball serve`；`cd frontend && npm run dev`。
4. 登录 → 上传一个 `.docx` → 在文件面板打开 → 编辑器以**编辑态**打开（标题栏显示「OnlyOffice 编辑」）。
5. 修改内容 → 点保存（forcesave，status=6）或关闭文档（status=2）。
6. 回看 `data/<userID>/workspace/<file>`：原文件应被新内容覆盖（`ls -la` 看 mtime 变化）。
7. 再次打开同一文件 → 应看到最新内容（每次打开后端签发新随机 key，强制重转，不会看到旧缓存）。
8. 出问题时看后端日志：`workspace.onlyoffice_callback.host_rejected`（siteUrl 与 server_url host 不符）或 `workspace.onlyoffice_callback.persist`（下载/写盘失败）。
