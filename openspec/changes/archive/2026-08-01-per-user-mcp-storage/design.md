## Context

per-user MCP（见 `user-mcp-configuration`）把用户私有 MCP 服务配置放在工作空间的 `.blowball/mcp/config.json`，所有 server 挤在一个 `servers` 数组里（`internal/tool/mcp/config.go` 的 `ConfigPath`/`LoadConfig`/`WriteConfig`）。`mcp_add_server` / `mcp_remove_server` 每次都要读-改-写**整份**数组；服务一多，文件冗长、并发编辑易冲突、单点原子写覆盖面大。

更关键的安全问题：当前 server `name` 仅校验非空（`Config.Validate`），它只是 JSON key。一旦改为"一个 server 一个子目录"的布局，`name` 就成了文件系统路径分量——`name="../etc"`、`name="a/b"`、`name=".ssh"` 之类会直接造成路径穿越、目录嵌套或覆盖。因此拆分目录必须同步引入严格的 name 校验。

本变更是后续 `mcp-tool-discovery`（端点重构 + `mcp_list_tools`）的地基：那个 change 的 `/mcp/tools` 端点要读用户 config 缓存、`mcp_list_tools` 的异步回写要落到 per-server 文件，都依赖此处稳定的按服务读写层。

## Goals / Non-Goals

**Goals:**
- 配置按服务拆分：`.blowball/mcp/{name}/config.json`，一服务一目录一文件。
- server `name` 作为目录名受到严格路径安全校验，杜绝穿越/嵌套/覆盖。
- 读（枚举服务）、增、删、单服务查询、单服务写都适配新布局，行为对外语义不变。
- per-server 文件独立原子写，缩小并发竞态窗口。

**Non-Goals:**
- **不**承担存量单文件 `config.json` → 多文件的迁移（部署侧另行处理；运行时不做双读兼容）。
- **不**修改 operator MCP（`config.yaml` 的 `mcp.servers`）路径。
- **不**改变传输（仍仅 http）、认证（仍仅静态凭据）、凭据隔离、turn-scoped 连接等既有不变量。
- **不**新增 HTTP API 或 agent 工具。
- **不**修改 `mcp_list_tools` 或 `/mcp/tools` 端点行为（属于 `mcp-tool-discovery`）。

## Decisions

### 1. 按服务拆分为 `.blowball/mcp/{name}/config.json`，新布局为唯一格式
- **Rationale**: 一个服务一个文件，天然支持并发编辑、缩小原子写覆盖面；与"一服务一连接一缓存"的心智模型一致。
- **Alternative**: 维持单文件 + 引入分段注释或 JSON Pointer。无法解决冗长与并发写覆盖问题。
- **Alternative**: 双读兼容（先扫子目录、再回退单文件）。会让 `LoadConfig` 长期背着双路径包袱，且与"不承担迁移"的非目标冲突。拒绝。

### 2. `name` 以目录名为准，不写入文件体
- **Rationale**: 目录名即身份，文件体只放 `url`/`transport`/`auth`/`description`/`tools`，消除"name 字段与目录名不一致"的二义性。`LoadConfig` 解析时把目录名回填为 `Server.Name`。
- **Alternative**: 文件体保留 `name` 字段并在加载时校验与目录名一致。增加一条无谓的不变量，收益低。

### 3. 严格 name 校验 `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`
- **Rationale**: `name` 成为路径分量后，必须禁止 `/`、`\`、前导 `.`、空格等。正则保证它是单一安全的路径段，且人类可读。
- **失效场景与防护**: 该校验在 `mcp_add_server` 入口、`LoadConfig` 加载期、`WriteConfig` 写前校验三处统一生效，任何非法 name 在落盘前即被拒。
- **对用户的影响**: `mcp_add_server` 能起的服务名受限（如不允许 `my server`、`a.b`、`../x`）。工具描述需点明命名规则。

### 4. 枚举服务 = 枚举 `.blowball/mcp/` 子目录
- **Rationale**: 不再维护一份顶层索引文件（那又会引入索引与实际文件漂移的问题）。`os.ReadDir` 只取目录项，跳过非常规名（如以 `.` 开头的临时文件、`atomicWriteFile` 的 `.mcp-config-*` temp）。
- **排序**: `SortedServers` 仍按 name 排序，保证 `mcp_list_servers` 输出确定性。

### 5. 写仍走 `atomicWriteFile`（temp + rename），但粒度是单 server 文件
- **Rationale**: 保持单次写崩溃安全（rename 原子）；粒度从"整份配置"降到"一个 server 文件"，并发写不同服务互不干扰，写同一服务的并发以 last-write-wins 收敛（仅影响缓存语义，可接受，详见风险）。

## Risks / Trade-offs

- **[Risk] 非法 name 导致 `mcp_add_server` 失败，影响既有使用习惯** → **Mitigation**: 工具描述明确命名规则（`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`）；错误信息给出具体规则与示例。该约束是路径安全的必要代价。
- **[Risk] 枚举子目录比读单文件多几次 I/O** → **Mitigation**: 用户服务数量通常为个位数到两位数，`os.ReadDir` + 少量小文件读开销可忽略；`.blowball` 本就是本地工作空间。
- **[Risk] 同一 server 的并发写（如 `mcp_list_tools` 异步回写与 `mcp_add_server` 撞车）丢失更新** → **Mitigation**: per-server 独立文件已把窗口压到单个 server；该文件本质是**缓存**（权威数据在远端 server 的 live `tools/list`），last-write-wins 最多导致缓存短暂 stale，下一次 `mcp_list_tools`/`mcp_call` cache-miss 会刷新。可接受，无需加锁。
- **[Risk] `os.ReadDir` 误把 temp/隐藏文件当服务** → **Mitigation**: 枚举时跳过非目录项、以 `.` 开头的条目，以及不通过 `ValidateName` 的目录名。

## Open Questions

- 是否需要在 name 校验之外额外维护一份"保留目录名"黑名单（如 `.blowball/mcp/cache/`）？当前 `.blowball` 整体已被 xizhi 拒绝访问、且 name 正则已排除点号开头，暂认为不需要，留待实现时确认。
