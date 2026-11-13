# Design: monorepo-restructure

## Context

仓库现状:单一 Go module `github.com/lush/blowball` 承载两个二进制——`cmd/blowball`(serve `--role all|api|agent` + seed)与 `services/scheduler/cmd/scheduler`。根 `internal/` 下 26 个包无服务边界,scheduler 生产代码导入其中约 10 个(rpcx、model、schedule、scheduledcontract、pkg/logger、config、api/gen/go、scheduledruntime、modelselect、scheduledcontrol),`internal/pkg/`(jwt/logger/trace/reqctx/cursor/flush)已是共享包雏形。office-vers 是上级目录的独立仓库(module 裸名 `office-version-service`,18 个 go 文件、自引用 9 处、git 历史仅 2 个 commit),blowball 通过 HTTP 调用它(artifact 通道)。部署文件散落:根 Dockerfile(单镜像双角色)、根 docker-compose.yaml(仅 mysql+redis)、`deploy/redis-ha/`、`deploy/scheduler/*.service`、office-vers 自带 Dockerfile;无任何 Jenkinsfile。

关键前提:代码库本身已是多进程安全设计——MySQL 权威、Redis claim/事件流负责跨进程协调、路由在 `internal/handler/router.go` 硬分区(`RegisterAPIRoutes`/`RegisterAgentRoutes`)、mTLS 服务身份 CN 已含 `blowball-api` 与 `blowball-agent` 两个独立身份。方案 C(api/agent 完全独立服务)是把既有角色分区提升为一等服务形态,不是新架构。

## Goals / Non-Goals

**Goals:**
- 仓库结构成为多服务 monorepo 的明确契约:四服务目录、共享 `pkg/`、服务私有 `internal/`。
- api 与 agent 进程、镜像、发布完全独立;数据面(MySQL/Redis/共享数据卷)保持共享。
- 部署工件集中于 `deploy/`:每服务 Dockerfile/Jenkinsfile/部署片段 + 全家桶 compose。
- office-vers 纳入仓库(import 重写、module 合并、openspec 并根)。
- 全程零功能影响:HTTP 契约、端口、配置格式、数据 schema、运行时行为不变。

**Non-Goals:**
- 不做存储分家(turn 引擎与工具层直接读写 sessions/工作区文件,分家等于重写)。
- 不拆 go module、不引入 go.work。
- 不改 HTTP 契约、配置 schema、migrations 内容。
- 不动 blowball-frontend 仓库。
- 不在本期承诺镜像极限优化(distroless/scratch 等,P3 可选)。

## Decisions

### D1. 独立的边界:进程与交付物独立,数据面共享

api/agent 拆为独立二进制与镜像,但 MySQL、Redis、`data/` 数据目录仍为共享依赖。已验证的共享事实:api 分区持有 workspace 文件 HTTP CRUD/上传下载/onlyoffice 回调/artifact 版本读取,agent 在 turn 中经 xizhi 工具读写同一批 `data/{userID}/workspace` 文件——两个容器必须挂同一数据卷。scheduler RPC 方向自然分家:api→scheduler(任务管理),scheduler→agent(`SubmitScheduledRun`)。

*Alternative 考虑过*:薄入口共享镜像(方案 B,`--role` 保留、镜像不分家)——被否决,用户要求完全独立交付;存储分家(真·微服务)——与"不影响功能"冲突,turn/tools 直读直写存储。

### D2. 单根 Go module,不引入 go.work

全部服务留在根 `go.mod`(module path 不变)。理由:api/agent 共享 DB schema(migrations)与 proto 契约,单 module 保证四个镜像永远出自同一 commit,对数据面强耦合的服务对是优点;跨服务重构一次 `go build ./...` 全量验证;Docker 构建无需处理 workspace。嵌套 go.mod 禁止(office-vers 的 go.mod 合并后删除)。

*Alternative*:每服务独立 go.mod + go.work——"独立性"由镜像与部署文件体现已足够,多 module 带来 replace 维护与版本漂移风险,在此规模(单团队四服务)是纯开销。

### D3. 共享包在 `pkg/`,服务私有在 `services/<svc>/internal/`

跨服务共享代码迁至根 `pkg/`(store/model/config/tokens/rpcx/schedule/scheduledcontract/logger/jwt/trace/reqctx/cursor/flush 等,含现有 `internal/pkg/` 并入);仅单服务使用的包移入该服务 `internal/`。Go 编译器强制 `services/<svc>/internal/` 只能被本服务目录导入——这是真正的服务边界,无需额外工具。归属判定依据导入关系数据(生产代码被 ≥2 服务导入 ⇒ pkg;仅测试导入不算)。`api/`(openapi+proto+gen/go)与 `migrations/`(server/scheduler 共库)留在根。

*Alternative*:共享代码留根 `internal/`——单 module 下语义等价(全 module 可见),但按用户明确偏好与"显式共享"意图选 `pkg/`。

### D4. 服务装配:导出装配包 + 薄 main + 单体组合

每个服务提供导出的装配包——位于服务目录根部的 `services/apiserver`(package apiserver)与 `services/agent`(package agentserver)——封装本服务的依赖构造与路由注册;`services/*/cmd/blowball-api|blowball-agent` 是 role 固定的薄 main,服务私有的 serve 生命周期在 `services/*/internal/run`;根 `cmd/blowball` 单体组合两个装配包实现 `--role all|api|agent` 与 seed。共享启动层为 `pkg/boot`(Runtime+Setup),版本元数据在无依赖叶子包 `pkg/buildinfo`(scheduler 边界不许主服务包,boot 会被边界测试拒绝)。这解决了 Go internal 可见性规则下"单体不能导入两个服务的 internal"的问题——单体只依赖导出的装配包。现有 `serve.go` 的集中 wiring 按角色拆归各服务装配包。

新二进制的 CLI 约定:接受与单体一致的 `-f/--config`、`-d/--data-dir`;不提供 `--role`(传入即非零退出,防误配);`blowball-api` 附带 `seed` 子命令(用户管理属 api 职责)。

### D5. office-vers 直接搬入,历史不保留

源码 `git` 语义上直接迁入 `services/officevers/`(cmd + internal/{config,handler,minio,router}),import 路径从 `office-version-service/...` 重写为 `github.com/lush/blowball/services/officevers/...`(9 处),依赖(gin、minio-go)并入根 go.mod(go 1.26.2 版本一致,无冲突),其 go.mod/go.sum 删除。git 历史仅 2 个 commit,不保留(旧仓库留档即可);不使用 `git subtree`(为 2 个 commit 背 subtree 元数据的长期成本不值)。其 openspec specs(`version-rollback`、`versioned-file-storage`,与根 69 个 capability 无命名冲突)原名义并入根 `openspec/specs/`。运行时零变化:URL、端口(:8088)、容器拓扑不动。

### D6. deploy/ 布局:每服务独立目录 + 全家桶 compose,仓库根 build context

```
deploy/
├── apiserver/  { Dockerfile(薄), Jenkinsfile, deploy.yaml }
├── agent/      { Dockerfile(厚), Jenkinsfile, deploy.yaml }
├── scheduler/  { Dockerfile, Jenkinsfile, blowball-scheduler.service }
├── officevers/ { Dockerfile, Jenkinsfile }
├── compose/all-in-one.yaml
└── redis-ha/   (现有内容平移)
```

Dockerfile 统一经 `docker build -f deploy/<svc>/Dockerfile .` 构建,context 恒为仓库根;`.dockerignore` 补齐 `data/ logs/ tmp/ bin/ .git` 等运行时产物。全家桶 compose 拉起四服务 + mysql + redis,共享数据卷 `blowball-data` 同时挂 apiserver 与 agent(workspace 文件双向可见是硬约束),office-vers 以服务名接入。现有根 Dockerfile/docker-compose.yaml 迁移拆分后删除,不保留双份。

### D7. 镜像策略:薄 api、厚 agent

- `blowball-api`:静态二进制 + ca-certificates + tzdata,不含 bwrap/python/node 等 agent 运行时依赖(现大镜像的依赖几乎全是 agent 角色的)。
- `blowball-agent`:维持现有运行时依赖全集(bubblewrap、python3、py3-pip、nodejs、npm、git、bash、fd、ripgrep,alpine 源镜像与 musl 静态构建不变)。
- `blowball-scheduler` / `officevers`:延续各自现状风格的最小 alpine 运行时。
- 四服务统一延续 office-vers 的版本注入习惯:`--build-arg VERSION/COMMIT/BUILD_TIME` → ldflags。

scheduler 二进制由 `scheduler` 规范化更名为 `blowball-scheduler`(与 blowball-api/blowball-agent 命名对齐),`deploy/scheduler/blowball-scheduler.service` 同步更新——这是本次唯一允许的对外可见命名变化。

### D8. 分期落地,每期独立可验收

P1 纯重组(目录 + import + officevers + openspec 并根 + deploy/ 骨架)→ P2 拆二进制 + deploy 全套 + 全家桶冒烟 → P3 可选优化(api 镜像极限瘦身、config 按服务裁剪)。详见 tasks。

## Risks / Trade-offs

- [import 重写遗漏或编译破坏] → 全程 `git mv` + 机械重写;每步门槛 `go build ./... && go vet ./... && go test -race ./...` + `test/integration` 全绿。
- [共享数据卷配置错误导致 agent 写的文件 api 读不到(或反之)] → 全家桶 compose 冒烟专测该链路:上传 → turn 内 xizhi 写文件 → api 分区读取 → artifact 版本解析。
- [双镜像版本漂移(api 新 agent 旧,共享 schema 不匹配)] → 单 module 单 commit 构建;Jenkins 流水线对同一 tag 构建全部触达服务。
- [wiring 拆分引入行为回归] → 单体(`--role all`)行为不变作对照;独立二进制与单体对应角色的路由/端口一致性纳入验证。
- [scheduler 改名破坏存量运维脚本] → `deploy/scheduler/blowball-scheduler.service` 同步更新,迁移说明显式列出该改名。
- [build context 膨胀拖慢构建] → `.dockerignore` 维护为 deploy 契约的一部分。
- [office-vers 依赖与主 go.mod 冲突] → 仅 gin + minio-go 两个直接依赖,go 版本一致,合并前 `go mod tidy` 验证。

## Migration Plan

1. **P1**:纯目录重组。`git mv` 全量移动 + import 重写 + office-vers 迁入 + openspec 并根 + deploy/ 骨架(迁移现有 Dockerfile/compose 至对应服务目录)。验证:全量测试绿;镜像从新路径构建,内容物与行为不变。生产零感知(镜像 tag 语义不变)。
2. **P2**:拆服务。新增 `blowball-api`/`blowball-agent` 薄 main + 装配包拆分;scheduler 更名 `blowball-scheduler`;per-service Jenkinsfile 就位;全家桶 compose 冒烟(HTTP 契约/端口/数据与单体逐项对照)。生产切换可选:可继续跑单体镜像过渡,独立二进制为增量交付,两者并行不冲突。
3. **P3(可选)**:api 镜像进一步瘦身、config schema 按服务裁剪(如 api 不再警告缺 openai key)。另行立项。

**回滚**:每期独立提交,可独立 `git revert`;P2 后生产仍可跑单体镜像(`--role all`/双容器单镜像),独立二进制不构成依赖。

## Open Questions

- Jenkins 的 registry 地址与凭据命名——Jenkinsfile 以参数/环境变量承载,实施时填入,不阻塞结构工作。
- 裸机 systemd 部署形态是否长期保留——本期维持并存(deploy/scheduler 的 unit 更新路径),不删除。
