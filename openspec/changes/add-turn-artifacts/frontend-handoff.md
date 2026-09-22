# 前端交接：产物链接与产物面板（add-turn-artifacts）

> 配套后端 change：`openspec/changes/add-turn-artifacts/`（proposal / design / specs / tasks）。
> 本文档是前端唯一需要读的文件；后端接口与事件契约以 `api/openapi.yaml` 为准（随 change 更新）。

## 目标

1. agent 回复中提到的交付物文件渲染为**可点击链接**，点击直接在工作区内打开（office 文件默认**预览**，非编辑）。
2. 每个 turn 产出的文件聚合为该消息下的**产物条**（chip 行），点击同样可打开。
3. **旧消息里的链接/chip 打开旧版本**——文件被后续 turn 覆盖后，历史引用不失真。

## 心智模型

```
agent 回复文本                       SSE 事件流（每个 turn）
┌─────────────────────────┐        ┌──────────────────────────────┐
│ …已生成 [季度报告.docx]   │        │ artifact {path, version_id,   │
│ (blowball://workspace/   │   +    │          size, mime, op} × N  │──▶ 产物条
│  reports/季度报告.docx)   │        │ done {meta.artifacts: [...]}  │
└─────────────────────────┘        └──────────────────────────────┘
     文本内链接（A）                     结构化产物事件（C）
              └──────── 两者点击后走同一个「打开文件」动作 ────────┘
```

- 链接里**永远没有版本参数**；版本（`version_id`）只存在于 artifact 事件里。
- 钉版是**渲染时解析**：链接 + 该消息所属 turn 的 artifact 列表 → 得出要打开的版本。

## 工作项 1：markdown 链接拦截

后端 prompt 约定 agent 用如下语法引用交付物：

```markdown
[季度报告.docx](blowball://workspace/reports/季度报告.docx)
```

要做的事：

1. **markdown 渲染器放行 `blowball:` scheme**。常见 sanitize 配置（如 rehype-sanitize）默认剥离未知 scheme，需要把 `blowball` 加入允许的协议列表，否则链接会被渲染成纯文本。
2. **拦截点击**，不走浏览器跳转。解析 URL：`blowball://workspace/<path>` → 取出 `<path>`（URL decode）。
3. 点击后进入「打开文件」流程（工作项 3），携带 `path` 与按下方算法解析出的 `versionId`（可能为空）。

注意：路径含中文/空格时先做 URL decode；agent 可能不写链接只写裸路径（依从性问题），v1 **不做**裸路径 linkify，裸路径就按普通文本显示——产物条（工作项 2）是兜底。

## 工作项 2：产物条（artifact chips）

### 实时流

turn 的 SSE 流末尾、`done` 之前会出现 0..N 个 `artifact` 事件：

```
event: artifact
data: {"type":"artifact","agent":"Confucius",
       "content":"{\"path\":\"reports/季度报告.docx\",\"version_id\":\"0192…\",\"size\":48213,\"mime\":\"application/vnd.openxmlformats-officedocument.wordprocessingml.document\",\"op\":\"create\"}"}

event: done
data: {"type":"done","meta":{"usage":{…},
       "artifacts":[{"path":"reports/季度报告.docx","version_id":"0192…","size":48213,"mime":"…","op":"create"}]}}
```

- `content` 是 JSON 字符串，字段：`path`（工作区相对路径）、`version_id`、`size`、`mime`、`op`（`create`|`update`）。
- `done.meta.artifacts` 是同内容摘要数组，可以直接用它渲染产物条（实时场景不需要自己收集 artifact 事件）。
- 产物条渲染在该 turn 最后一条 assistant 消息下方：每个产物一个 chip（文件图标 + 文件名 + 大小），点击 →「打开文件」流程，携带 `path` + `version_id`。
- 无产物时 `artifacts` 为空数组，不渲染产物条。

### 历史消息还原

`artifact` 事件已随消息持久化（`event_type = "artifact"`），拉历史消息时会拿到。按 turn（同一 run 的消息组）归属到对应消息下即可。`done` 不持久化，**历史场景必须从 artifact 事件行重建产物列表**，不要找 done。

### 消息时间

每条消息的 `msg_time` 字段用于跨 turn 钉版（见下）。

## 工作项 3：「打开文件」动作路由

输入：`path` + `versionId`（可空）。按扩展名路由：

| 类型 | 扩展名 | 当前版本（versionId 为空） | 历史版本（有 versionId） |
|---|---|---|---|
| Office | doc docx xls xlsx ppt pptx csv | `GET /api/v1/workspace/files/{path}/onlyoffice-config` → 用响应里的 **`view`** 配置初始化 OnlyOffice | `GET /api/v1/workspace/versions/{versionId}/onlyoffice-config` → 初始化 OnlyOffice（本就只读） |
| PDF | pdf | iframe/embed 指向 `GET /api/v1/workspace/files/download/{path}?token=<jwt>` | iframe 指向 `GET /api/v1/workspace/versions/{versionId}/content?token=<jwt>` |
| 图片 | png jpg jpeg gif webp svg | 同上，`<img>` | 同上 |
| 文本/代码 | md txt json go py … | `GET /api/v1/workspace/files/{path}/content`（Bearer）→ 应用内查看器 | `GET /api/v1/workspace/versions/{versionId}/content`（Bearer）→ 同一查看器 |
| 其他 | — | 下载：`/api/v1/workspace/files/{path}` | 下载：`/api/v1/workspace/versions/{versionId}/content` |

要点：

- **office 默认预览**：现有 onlyoffice-config 响应已同时返回 `edit` 和 `view` 两套签名配置，默认用 `view` 即可；是否提供"切换到编辑"按钮由产品决定。
- OnlyOffice 初始化：用响应的 `server_url` + config + token 调 Docs API（与现有工作区打开文件的逻辑完全相同，只是默认模式不同）。
- `?token=` 形式用于无法带 Authorization 头的浏览器上下文（`<img>`/iframe/OnlyOffice document.url），token 即用户 JWT。

## 版本钉定算法（渲染消息中的链接时）

对消息 M 中出现的每个 `blowball://workspace/<path>` 链接：

```
1. 在 M 所属 turn 的 artifact 列表里按 path 查找
   → 命中：用该条目的 version_id
2. 未命中（模型引用了更早 turn 产出的文件）：
   → GET /api/v1/workspace/versions/resolve?path=<path>&before=<M 的 msg_time>
   → 200：用返回的 version_id；404：versionId 留空
3. versionId 为空 = 打开"当前版本"
```

`resolve` 结果建议按 `(path, before)` 做会话级缓存。失败兜底：一律降级为打开当前版本，不要阻断点击。

## 错误与边界

| 情况 | 表现 | 处理 |
|---|---|---|
| 链接/版本 404 | 文件被删或无此版本 | toast"文件不存在或已被删除" |
| onlyoffice-config 503 | 部署未配置 OnlyOffice | 降级为下载 |
| 流式渲染中途遇到链接 | 产物尚未出现在 done 摘要 | 先按"当前版本"渲染，done 到达后用 artifacts 重新钉版 |
| SSE 断线重连 | 事件重放 | artifact 事件按 `(path)` 去重 |
| 链接指向 tmp/ 或越界路径 | 后端接口会 403/404 | 按 404 处理即可 |

## 验收清单

- [ ] markdown 渲染器不剥离 `blowball:` 链接，且普通 https 链接行为不变
- [ ] 点击文本内链接：docx 进 OnlyOffice **预览**；pdf/图片内联预览；文本进查看器；其他触发下载
- [ ] 每条产物 chip 可点击，行为与文本链接一致
- [ ] turn N 生成的文件在 turn N+2 被覆盖后：turn N 消息里的链接和 chip 打开**旧内容**，工作区里打开是**新内容**
- [ ] 刷新页面/重进会话后，产物条与钉版行为不变
- [ ] 无产物的 turn 不渲染产物条；`done.meta.artifacts` 为空数组
- [ ] 引用他人 versionId 返回 404（越权不可见）
