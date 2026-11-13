# 包归属划分表(Task 1.1/1.2 产出)

方法:`go list` 构建模块导入图;scheduler 生产依赖取自 `go list -deps ./services/scheduler/cmd/scheduler`;api/agent 角色闭包从 `internal/handler` 按路由注册体(RegisterAPIRoutes/RegisterAgentRoutes 引用的 handler 符号)分类文件后计算;wiring 层直接构造的包(llmraw、scheduledcontrol、scheduledruntime、storage、tool/executor、tool/scheduledtask)按 serve.go 角色分支归属复核。

## 结论

**数据判定:根 `internal/` 全部 27 个包(含 `internal/pkg/` 6 个子包)均为跨服务或跨角色共享,全部迁入根 `pkg/`。** P1 不产生任何 `services/apiserver/internal` 或 `services/agent/internal` 归属包。

依据:

| 包 | scheduler | api 角色可达 | agent 角色可达 | 归属 |
|---|---|---|---|---|
| config, model, rpcx, schedule, tokens, pkg/logger, pkg/reqctx, pkg/trace | ✅ | ✅ | ✅ | pkg/ |
| scheduledcontract | ✅ | ✅(经 scheduledcontrol) | — | pkg/ |
| handler, middleware, service, store/{mysql,fs,redis}, stream, modelselect, agent, tool/*, turn, run, artifact, memory, prompt, resource, skillmarket, mcpmarket, pkg/{cursor,jwt} | — | ✅ | ✅ | pkg/ |
| llmraw, pkg/flush | — | —(api 角色显式 nil) | ✅ | pkg/(仅 agent wiring 构造,但无第二个服务 internal 可放——见下) |
| scheduledcontrol | —(仅测试) | ✅ | — | pkg/ |
| scheduledruntime | —(仅测试) | ✅(all 角色) | ✅ | pkg/ |
| storage | — | ✅ | ✅ | pkg/ |
| tool/executor, tool/scheduledtask | — | —(仅 agent wiring) | ✅ | pkg/(同 llmraw) |

关键发现:

1. **角色差异在 wiring 层,不在包依赖层。** `internal/handler` 是单包双分区(RegisterAPIRoutes/RegisterAgentRoutes 同包),api 分区经 workspace/artifact/subagent/scheduled-session 处理器触及几乎所有"agent 侧"包(如 xizhi、artifact、turn 兼容层)。包粒度专属拆分不存在。
2. **serve.go 已有 `wireAPI`/`wireAgent` 按角色分区的构造函数**——api/agent 服务化的实现路径是提取这两个函数为 `services/apiserver`/`services/agent` 的装配包(P2/任务 5.1),而非移动业务包。
3. **llmraw/flush、tool/executor、tool/scheduledtask 仅 agent wiring 构造**,但同被根 `test/integration`(19 个内部包导入)与 handler 共享面引用;按"≥2 服务生产代码共享 ⇒ pkg"规则与 internal 可见性约束(root 的 cmd/test 不能导入 services/*/internal),归 pkg/。将来若要真正收紧为 agent 专属,需先拆 handler 包,超出本次零功能影响范围。
4. `api/`(openapi/proto/gen)与 `migrations/` 留根(跨服务契约/共库),不变。

对任务清单的影响:任务 2.2 原文"主服务包按归属迁入 services/apiserver/internal 与 services/agent/internal"按本表修正为"主服务包全部迁入 pkg/(数据判定无服务专属包);services/apiserver、services/agent 目录在 P2 任务 5.1/5.2 随装配拆分创建"。设计 D3 规则不变,本表是其数据结论。
