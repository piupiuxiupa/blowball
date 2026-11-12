## 1. 审阅与传输契约

- [x] 1.1 审阅本提案的工程默认值：单 Go module、主服务持有任务会话、资源发布版本、start_intent 后保守恢复及 execution hold、任务会话只读；确认后再开始应用代码。
- [x] 1.2 在 api/proto 下定义 scheduler/v1 与 scheduledruntime/v1 的管理、actor/模型校验、资源、历史、接收/查询/取消和 start grant RPC；明确 ID、source、revision、fingerprint、deadline 与错误码，不包含用户凭证。
- [x] 1.3 添加兼容 Go 1.26 的锁定 gRPC/protobuf 依赖及可重复生成目标；生成客户端，测试字段往返、版本兼容和敏感字段排除。
- [x] 1.4 先写 mTLS/每方法服务身份、伪造 actor、deadline 和 peer 不可用测试，再实现公共 RPC 中间件及客户端连接复用。

## 2. 数据迁移与物理隔离

- [x] 2.1 编写增量 migration：任务资源引用/历史来源/只读 execution hold 字段、scheduled_sessions、消息/工具/标题/用量/压缩/子 Agent/附件及需要的归档表、资源版本元数据和 execution receipts；验证 FK、索引及唯一 occurrence/run 约束。
- [x] 2.2 先写普通/任务 repository 隔离与幂等建会话测试，再实现固定来源的存储适配；拒绝创建普通占位 session，禁止客户端表名和跨表 fallback。
- [x] 2.3 适配任务 round 双表事务、send-time 和 finalization；测试新数据无 chunk 行、工具顺序和终态 receipt 不先于最终内容，不复活 ingest queue。
- [x] 2.4 适配标题、用量、压缩标志/锚点/工具统计、子 Agent instances/transcripts 和附件；测试所有任务写入只落 scheduled 表。
- [x] 2.5 适配读取前 owner/source 校验、缓存回填、任务删除及用户删除归档；验证 stale cache、跨用户 404、运行中删除 409 和普通数据不受影响。

## 3. 主服务任务执行入口

- [x] 3.1 先写稳定身份重复接收、fingerprint 冲突、并发 RPC 和接收响应丢失测试，再实现 receipt+scheduled session 的事务幂等接收。
- [x] 3.2 实现 Runtime 对权威 occurrence、owner active、模型和资源引用的校验；仅在本地身份链路解析/签发执行凭证，测试秘密不进入 RPC、数据库、日志或工具。
- [x] 3.3 复用现有 turn 入口并注入已校验的 scheduled 存储上下文；测试状态/预算 cache-only、压缩及子 Agent 持久化不串表。
- [x] 3.4 实现 receipt worker CAS/lease、start_intent、后台 fail-closed admission 和 Scheduler start grant 调用；测试 stale generation、授权响应丢失、暂停竞争与第一项副作用前的拒绝。
- [x] 3.5 实现从 grant 计时的 Runtime watchdog、显式取消和终态事务；测试 Scheduler 离线时超时仍保存部分内容及 TASK_TIMEOUT。
- [x] 3.6 实现权威 GetScheduledExecution、停止证明和保守 receipt 恢复；测试 accepted/preparing 的安全恢复、start_intent 后不重放、uncertain 不伪装已停止、迟到 worker/终态不能覆盖新状态。
- [x] 3.7 适配记忆所需的任务身份/历史读取并保持现有开关、授权和策略；测试 cache-only 状态/资源元数据不被当作用户原文，以及普通会话记忆回归。

## 4. 独立 Scheduler 服务

- [x] 4.1 创建 services/scheduler 的独立入口/配置/任务业务/专属 Store 和 gRPC server；保持根 module，测试依赖图不含 Agent、全量主服务 Store、LLM、MCP 或 workspace/executor。
- [x] 4.2 将任务 CRUD、纯时间规则及 immutable snapshot 管理移入 Scheduler；测试原输入/模型/at/cron/timezone、权限、状态码和可选 expected_revision。
- [x] 4.3 迁移物化、lease/claim、三次有界重试和任务不重叠逻辑；测试多副本唯一槽位、漏 cron 合并、已消费 at 不重放及事务回滚。
- [x] 4.4 实现 AuthorizeStart/GetStartGrant：有效 claim/attempt、任务启停、唯一 receipt generation、started_at/deadline 和同身份幂等授权；测试 pause/delete 与启动事务排序。
- [x] 4.5 使用 Runtime RPC 提交及对账，不传进程内 BeforeStart/OnTerminal；测试接收/授权/终态响应丢失、权威状态不可达时无盲目重跑。
- [x] 4.6 实现重启对账、持久化 task-level execution hold、停止证明匹配/CAS 清除、容量和停机释放；测试 uncertain/hold 计入容量、后续槽位不重叠、用户恢复不能绕过 hold、Scheduler 停机不取消已 accepted 工作。

## 5. `.cron` 资源与写保护

- [x] 5.1 先写 namespace owner、未受管理目录冲突、草稿与发布区规则测试，再实现 .cron/<task_id> 的受控初始化/解析；不覆盖现有用户文件。
- [x] 5.2 实现 draft revision 与跨 API 节点写入/发布围栏，接入文本写入、上传、创建、删除和 rename；测试并发编辑、失效围栏和受保护祖先/移动目标。
- [x] 5.3 实现受控 staging、普通文件校验、排序 manifest、实际字节 SHA-256、容量上限、原子发布与幂等 publication 元数据；测试 crash/重试/超限不产生可绑定半成品。
- [x] 5.4 实现资源 get/publish/validate RPC 与 Task 资源引用 CAS 绑定；测试空资源任务、disabled 准备流程、跨用户绑定和发布成功但配置更新失败。
- [x] 5.5 适配文件工具和 Office 写回的管理路径保护；测试 symlink/路径竞态、发布版本直接覆盖、任务根删除和 `.cron` 之外现有文件行为。
- [x] 5.6 在所有 bash 中增加 .cron 只读视图，处理尚未初始化的命名空间；测试 argv/挂载不改变既有技能/env/network/其他 workspace，并在 Linux 用真实 bwrap 验证旁路写入失败。
- [x] 5.7 实现执行前 pinned manifest/内容校验及最小资源上下文；测试不读 draft/latest、不自动执行附件、不改 Task.prompt 原文及预算估算。
- [x] 5.8 实现受控 staging/未引用版本维护及保留引用检查；测试 Task/session 删除和 active Run 不清理被引用资源，多节点发布/执行沿用共享 workspace。

## 6. HTTP、工具及历史合同

- [x] 6.1 将现有任务 handler 改为 Scheduler RPC 适配，保留字段和成功状态码；测试 Scheduler 不可用返回 503、无本地 fallback，普通聊天不受影响。
- [x] 6.2 添加 scheduled-sessions 列表、详情、消息分页、删除及 Agent 角色 Run cancel 路由；测试独立列表、跨用户 404、普通接口拒绝 scheduled ID 和不存在自由对话入口。
- [x] 6.3 扩展 Task/Occurrence DTO，返回 expected revision、resource 引用、历史快照、实际模型、session_source 及只读阻塞原因；测试历史编辑只修改当前 Task且不能清除 execution hold。
- [x] 6.4 添加资源元数据/发布 HTTP 接口，复用主服务资源业务及 owner/revision 校验；测试 request ID 幂等与冲突错误。
- [x] 6.5 注册 scheduled_task 管理及 resource get/publish 工具，使用可信执行上下文和相同业务；测试伪造 user、跨用户 task、HTTP/工具校验一致、未配置/离线行为。
- [x] 6.6 更新 api/openapi.yaml，完整描述路径、角色、分页、来源、配置/快照、revision、400/401/403/404/409/503 及不再允许 existing 的迁移说明。

## 7. 配置、角色与部署

- [x] 7.1 从 all/agent wiring 移除调度构造和循环，按角色注册内部 RPC；测试 all 零内嵌 Scheduler、API 无 LLM 依赖、普通 HTTP 路由/端口回归。
- [x] 7.2 添加独立 Scheduler 配置和主服务 RPC endpoint/TLS 配置示例；正值校验默认 5s/4/10m，明确旧 enabled 配置迁移；示例秘密使用环境变量。
- [x] 7.3 添加 Runtime scheduled admission 上限、receipt/授权/发布可观测性和就绪检查；验证不是普通聊天 CPU/内存配额承诺，RPC 依赖故障不错误宣告执行成功。
- [x] 7.4 增加独立 build/run/deploy 目标及服务身份/数据库写权限边界，延续现有共享存储健康检查；构建产物不得进入 Git。

## 8. 遗留任务与历史迁移

- [x] 8.1 编写只增加字段/表的迁移和演练，遗留 history 显式标记 legacy_interactive；测试旧普通会话不搬表、不隐藏、不创建 scheduled 副本。
- [x] 8.2 停旧循环并排空/保守处理旧 non-terminal，无法证明停止时建立 Task hold；迁移 new 策略的未来触发，暂停 existing 并要求显式确认；测试不重播槽位、改变旧上下文或与未停旧 worker 重叠。
- [x] 8.3 实现按明确来源读取旧任务历史和内容已删除标记；测试没有跨表猜测，Task 软删除后仍可从专属历史入口读取保存事实。
- [x] 8.4 更新 docs/scheduled-tasks.md、配置/部署说明及回滚步骤；验证回滚只关闭新调度、保留新表/资源/读服务，不启动旧内嵌实例或重放未知 Run。

## 9. 故障矩阵与验收

- [x] 9.1 增加跨进程集成测试：多 Scheduler/Runtime 接收、响应丢失、start grant 竞争、各持久化点崩溃、Redis/Scheduler/RPC 不可用和迟到 worker；覆盖跨 occurrence 的未知执行 hold，断言停止证明前不启动后续任务且无自动重放不确定 Agent。
- [x] 9.2 增加任务表组全链路测试：工具/子 Agent、压缩、取消/超时、缓存 miss、终态/删除归档，断言普通表零新任务数据及 canonical 状态不串存储。
- [x] 9.3 增加跨 API/Agent 的资源并发/故障与权限测试，覆盖保护祖先、真实 Linux bwrap、共享 workspace、固定版本和凭证泄露检查。
- [x] 9.4 运行 OpenSpec strict 校验、OpenAPI/生成产物检查、go vet、构建与 make test；记录 Linux/外部依赖测试前置条件，不以跳过冒充通过。
- [x] 9.5 每个完成的自包含阶段先运行对应测试再创建 scoped Conventional Commit；最终核对仅提交源代码、测试、文档与配置示例，不提交 bin、秘密、日志或运行数据。

## 10. 审查回归修复

- [x] 10.1 补齐主服务 shared turn 的 scheduled sessions/messages/titles/compaction/snapshots 接线，保留缺少存储依赖时 fail-closed，并添加生产构造/执行入口回归测试。
- [x] 10.2 在释放 occurrence active 状态前可靠保存 execution hold；hold 写入失败保留非终态以便对账重试，覆盖提交顺序及失败恢复窗口。
- [x] 10.3 修正启动前 receipt 的 status/reason 参数绑定；真实 MySQL 读回验证可识别终态、原始原因与 not_started 停止证明。
- [ ] 10.4 修正 xizhi_write_file 的草稿写入操作类型，覆盖 write/append、授权草稿及发布区拒绝行为。
- [x] 10.5 修正 scheduled placeholder 分页的关联表别名，真实 MySQL 验证升序/降序、空页及普通/full 对照。
- [ ] 10.6 增量迁移补齐 scheduled_subagent_runs 的 create_time/update_time，兼顾既有行，覆盖 list/get/child/history 读回并更新迁移文档。
- [ ] 10.7 保留暂停/初始禁用的未来 at 槽位，覆盖恢复与已消费 at 不重放、cron 重新计算槽位；完成全量 race/lint/build 和 OpenSpec strict 验证。
