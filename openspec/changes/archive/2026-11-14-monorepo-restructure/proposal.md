# Proposal: monorepo-restructure

## Why

仓库目前是"主服务在根目录 + scheduler 在 services/ 下 + office-vers 在仓库外"的混合形态:主服务与 scheduler 共享一个 go.mod 却没有清晰的共享包边界,部署文件散落在根目录/Dockerfile、根 compose、deploy/、office-vers 各自仓库里,且没有任何 CI 流水线定义。api 与 agent 两个角色虽已在代码层硬分区(路由、端口、mTLS 身份),却始终以同一个二进制/镜像交付,层次不清且难以独立演进。

## What Changes

- **目录重组为多服务 monorepo**(单 go module 不变):主服务下沉为 `services/apiserver/` 与 `services/agent/` 两个一等服务目录,各自拥有独立 `cmd/`、导出的 `server` 装配包和私有 `internal/`;scheduler 平移至 `services/scheduler/`;office-vers 从上级目录整体纳入为 `services/officevers/`(import 重写,module 合并,git 历史不保留——仅 2 个 commit)。
- **公共包抽离到根 `pkg/`**:把跨服务共享的包(store/model/config/tokens/rpcx/logger/jwt/trace/schedule/scheduledcontract 等,含现有 `internal/pkg/`)迁至 `pkg/`,`services/<svc>/internal/` 由 Go 编译器强制服务私有边界。
- **api 与 agent 拆为完全独立服务(方案 C)**:新增 `blowball-api`、`blowball-agent` 两个薄入口二进制(role 固定),进程、镜像、发布完全独立;数据面(MySQL/Redis/共享数据卷)保持共享。根 `cmd/blowball` 单体(`--role all|api|agent` + `seed`)保留为开发模式,行为不变。
- **部署文件集中到 `deploy/`**:每服务独立目录存放各自的 Dockerfile、Jenkinsfile、systemd/compose 部署片段;新增 `deploy/compose/all-in-one.yaml` 全家桶 compose(四服务 + mysql + redis + 共享卷);api 镜像瘦身为静态二进制 + 最小运行时,agent 镜像维持现有厚依赖(bwrap/python/node 等)。
- **openspec 并根**:office-vers 的 `openspec/specs/`(version-rollback、versioned-file-storage,与根无命名冲突)并入根 `openspec/specs/`。
- **不改动的部分**:HTTP 契约、端口、配置文件格式、数据 schema、运行时行为全部不变;scheduler 二进制规范化命名为 `blowball-scheduler`(原 `scheduler`)。

## Capabilities

### New Capabilities
- `monorepo-service-topology`: 仓库即多服务 monorepo 的结构契约——services/ 四服务布局、单根 Go module、`pkg/` 共享包边界、`services/<svc>/internal/` 私有边界、根 `cmd/blowball` 开发单体、office-vers 仓库归属与 openspec 并根。
- `service-deployment`: deploy/ 部署工件契约——每服务 Dockerfile/Jenkinsfile/独立部署片段、全家桶 compose、薄 api 镜像/厚 agent 镜像策略、共享数据卷拓扑、版本注入构建参数、仓库根 build context。

### Modified Capabilities
- `service-roles`: 新增 `blowball-api`/`blowball-agent` 独立二进制(role 固定、路由/端口与单体对应角色完全一致),单体 serve 角色行为不变。
- `scheduled-task-service`: "首期保留根 Go module 与现有主服务入口"的过渡表述落地为终态布局;scheduler 构建入口仍在 `services/scheduler/`,二进制规范化命名 `blowball-scheduler`。

## Impact

- **代码移动**(git mv + import 重写,机械且可验证):`internal/` 大部分包拆分至 `pkg/` 与 `services/*/internal/`;`api/gen/go` 契约包位置不变;`services/scheduler/`、`cmd/blowball/` 结构微调;office-vers 18 个 go 文件迁入。
- **构建/部署**:Makefile 目标、根 Dockerfile 与 docker-compose.yaml 迁入 deploy/ 并按服务拆分;新增 Jenkinsfile(全新,无存量);镜像行为不变(binary 名/flag/配置路径不动,仅 `scheduler`→`blowball-scheduler` 规范化)。
- **文档同步**:CLAUDE.md、AGENTS.md、README(openapi.yaml 同步到 blowball-frontend 的路径指引)、docs/、openspec specs 中的路径引用。
- **不受影响**:前端仓库(HTTP 契约不变)、数据库与 migrations(位置不变,server 与 scheduler 共用的归属维持根目录)、生产部署拓扑(可从单镜像双容器无损切换,也可继续跑单体镜像过渡)。
