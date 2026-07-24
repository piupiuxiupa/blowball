## 1. 后端：双模式配置签发

- [x] 1.1 `internal/handler/workspace.go` 重构 `buildOnlyOfficeConfig`：生成一个共享随机 `key`，产出 edit 与 view 两个 config map（公共字段 `documentType`/`document.{fileType,title,url}`/`document.key` 相同；edit=`mode:"edit"`+`permissions.edit:true`+`forcesave:true`，view=`mode:"view"`+`permissions.edit:false`+无 `forcesave`；两者均含 `callbackUrl` 与同一用户 JWT）
- [x] 1.2 重构 `onlyOfficeConfigResponse` 结构体为 `{ServerURL, Edit{Config,Token}, View{Config,Token}}`（新增 `onlyOfficeModeConfig{Config,Token}` 子结构，JSON tag `edit`/`view`）
- [x] 1.3 `OnlyOfficeConfig` handler：对 edit/view 两个 config 分别 `jwt.SignClaims(h.oo.Secret, cfg)`，组装嵌套响应；503/403/404/400 错误路径保持不变
- [x] 1.4 确认 `OnlyOfficeCallback`、`onlyOfficePersist`、SSRF host 白名单、原子 rename 落盘**无任何改动**

## 2. API 文档

- [x] 2.1 `api/openapi.yaml` 重构 `OnlyOfficeConfigResponse`：`server_url` + `edit`/`view`，各含 `{config, token}`；更新 `config` 描述区分 edit/view 两种内容约束
- [x] 2.2 端点路径、鉴权（Bearer）、错误码（403/404/400/401/503）描述不变

## 3. 后端测试

- [x] 3.1 `internal/handler/workspace_test.go` 配置端点用例：响应含 `edit`+`view` 两套 `{config, token}`
- [x] 3.2 两 token 各自可用同一 secret 验签，payload 恰为对应 config，且**两 token 互不相同**
- [x] 3.3 edit/view 字段断言：edit=`mode:"edit"`/`permissions.edit:true`/含 `forcesave`；view=`mode:"view"`/`permissions.edit:false`/`download:true`/无 `forcesave`
- [x] 3.4 edit/view 共享同一 `document.key`
- [x] 3.5 错误路径不回归：路径越界 403、文件不存在 404、目录 400、未鉴权 401、未配置 503
- [x] 3.6 回调端点既有用例保持通过（不回归）

## 4. 前端（blowball-frontend 独立仓）

- [x] 4.1 复制 `api/openapi.yaml` 到 blowball-frontend，`npm run generate-api` 重生成 `openapi.d.ts`
- [x] 4.2 office 配置消费处由 `{config, token}` 改为按选定模式取 `resp.edit` / `resp.view`
- [x] 4.3 office-viewer 用对应模式的 `{config, token}` 实例化 `new DocsAPI.DocEditor`；支持运行时切换编辑/只读（重取配置即换新随机 key）

## 5. 验证

- [x] 5.1 `make lint && make test` 通过
- [x] 5.2 端到端（待人工）：配置 `onlyoffice` 段 → 打开 office 文件 → 以**编辑态**打开可编辑保存；切到**只读态**打开无编辑工具栏、无法改内容、关闭不触发落盘；两种态用各自 token 均能被 DocumentServer 验签通过
