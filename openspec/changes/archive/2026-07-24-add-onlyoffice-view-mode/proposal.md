## Why

OnlyOffice DocumentServer 用 JWT 对**整个 editor config** 做验签：`token = HS256(secret, config)`，浏览器侧 `new DocsAPI.DocEditor(id, {...config, token})`。这意味着前端**不能**在拿到 edit 配置后自行把 `mode` 改成 `"view"`——一旦改动 config，签名失效，DocumentServer 直接拒开。所以「同一个文件既要以编辑态打开、又要以只读态打开」必须由后端分别签发**两个**互不相同的 config 与对应 token。

当前 `GET /api/v1/workspace/files/*path/onlyoffice-config` 只返回单一 edit 配置（`{server_url, config, token}`，`mode:"edit"`、`permissions.edit:true`），前端若想切到只读预览（例如文件预览面板、避免误编辑），无法在不重新请求的情况下完成。本变更让该端点**一次性返回编辑与只读两套 (config, token)**，前端可即时在两种模式间切换，无需二次往返。

## What Changes

- **编辑器配置端点返回双模式**：`GET /api/v1/workspace/files/*path/onlyoffice-config` 的响应由 `{server_url, config, token}` 改为 `{server_url, edit:{config, token}, view:{config, token}}`。两套 config 共享**同一个随机 `document.key`**（同一文件、同一次打开、一个文档身份；随机 key 本身仍保证每次打开重转）。
- **edit config**（不变）：`mode:"edit"`、`permissions:{edit:true, download:true}`、`customization.forcesave:true`、`callbackUrl`。
- **view config**（新增）：`mode:"view"`、`permissions:{edit:false, download:true}`、**不含** `forcesave`；保留 `callbackUrl`（只读态只会触发 status=1/4 的非保存回调，既有回调 handler 以 `{"error":0}` 确认，复用同一用户 JWT，零额外成本）。
- **两个独立 token**：edit / view 各自用 `onlyoffice.secret` 对**各自** config 做 HS256，互不相同。
- **OpenAPI 契约**：`OnlyOfficeConfigResponse` schema 重构为 `edit`/`view` 嵌套；复制到 blowball-frontend 并 `npm run generate-api`。
- **非目标**：不引入「按用户/按文件」的编辑权限门控——当前工作区严格 per-user（`data/{userID}/workspace`），所有已鉴权用户都是文件 owner，两套 token 下发给 owner 即安全；未来若引入文件共享/只读 ACL，需另行让 edit token 按权限条件性下发（见 design.md 安全说明）。

## Capabilities

### Modified Capabilities
- `office-file-editing`：将「签发已签名的 OnlyOffice 编辑器配置」需求改为「签发编辑+只读两套已签名配置」，响应体结构变更、新增只读 config 的内容约束、保留共享随机 key 与全部既有错误路径（403/404/400/401/503）。

## Impact

- **后端**：`internal/handler/workspace.go` 的 `OnlyOfficeConfig` handler 与 `buildOnlyOfficeConfig` 重构——生成一个共享随机 key，构造 edit/view 两个 config，各自 `jwt.SignClaims` 签名，返回嵌套结构 `onlyOfficeConfigResponse{ServerURL, Edit{Config,Token}, View{Config,Token}}`；503（未配置）/403（越界）/404（不存在）/400（目录）错误路径不变。`OnlyOfficeCallback` 与落盘逻辑**完全不动**。
- **API 文档**：`api/openapi.yaml` 的 `OnlyOfficeConfigResponse` 重构（`server_url` + `edit`/`view` 各含 `{config, token}`），端点路径与鉴权不变。
- **前端**（blowball-frontend 独立仓）：消费处由 `{config, token}` 改为按选定模式取 `edit`/`view`；office-viewer 据此用对应 config+token 实例化 DocEditor，支持运行时在编辑/只读间切换（重取配置即换新随机 key）。
- **配置/依赖**：无新增（`onlyoffice.{secret,server_url,internal_backend}` 复用，HS256 复用 `internal/pkg/jwt`）。
- **安全**：secret 仍只在后端；双 token 均为对各自 config 的签名，前端无法伪造或越权改 mode；SSRF host 白名单与原子落盘逻辑不受影响。
