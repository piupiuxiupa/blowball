# monorepo-service-topology Specification

## Purpose
定义 monorepo 的服务拓扑：单一 Go module 构建四个服务（dev 单体 `blowball` 组合各服务导出的装配包；`blowball-api`/`blowball-agent`/`blowball-scheduler` 与 officevers 版本服务各居 `services/`），跨服务共享代码全部位于根 `pkg/`，重组对功能面零影响、单体与独立二进制逐角色行为一致。
## Requirements
### Requirement: Four-service monorepo layout

仓库 SHALL 以 `services/` 为服务根,提供四个一等服务目录:`services/apiserver`(API 服务)、`services/agent`(Agent 服务)、`services/scheduler`(Scheduler)、`services/officevers`(office 版本服务)。每个服务目录 SHALL 拥有自己的 `cmd/` 入口与私有 `internal/`。api 与 agent 服务 SHALL 额外在服务目录根部提供导出的装配包(`services/apiserver`、`services/agent`)用于被开发单体组合;scheduler 与 officevers 为自足的 `cmd/`+`internal/` 服务,无需装配包。

#### Scenario: 服务目录结构完整
- **WHEN** 检查 services/ 目录
- **THEN** 存在 apiserver、agent、scheduler、officevers 四个服务目录,各自含 cmd 入口与 internal 私有包;apiserver 与 agent 暴露导出装配包

#### Scenario: 主服务代码不再位于仓库根
- **WHEN** 检查仓库根目录
- **THEN** 根目录不存在原主服务业务包(internal/ 下的 handler、agent、turn、tool 等已按归属迁入 pkg/ 或各服务 internal/)

### Requirement: Single root Go module

全部服务 SHALL 由仓库根的唯一 `go.mod` 构建,module path `github.com/lush/blowball` SHALL 保持不变。服务目录内 SHALL NOT 存在嵌套 go.mod。

#### Scenario: 单 module 构建全部服务
- **WHEN** 在仓库根执行 go build ./...
- **THEN** 四个服务的入口包(含根 cmd/blowball)全部编译成功,仓库内无第二个 go.mod

#### Scenario: office-vers 依赖并入根 module
- **WHEN** office-vers 迁入后执行 go mod tidy
- **THEN** 其依赖(gin、minio-go)由根 go.mod 承载,services/officevers/ 下无独立 go.mod

### Requirement: Shared package boundary

被两个及以上服务的生产代码共同依赖的包 SHALL 位于根 `pkg/`;仅单服务使用的包 SHALL 位于该服务 `services/<svc>/internal/` 下。`api/`(openapi/proto/gen)与 `migrations/` SHALL 保留在仓库根作为跨服务契约与共库迁移。服务 SHALL NOT 导入其他服务的 `internal/`(由 Go 编译器强制)。

#### Scenario: 编译器强制服务私有边界
- **WHEN** services/apiserver 下的代码导入 services/agent/internal/ 下任意包
- **THEN** 编译失败,越界导入不被接受

#### Scenario: 共享包位于 pkg
- **WHEN** 检查被 apiserver 与 agent(或 scheduler)共同导入的包位置
- **THEN** 这些包位于根 pkg/ 下,而非任一服务的 internal/

### Requirement: Development monolith retained

根 `cmd/blowball` SHALL 保留为开发用单体:`serve` 子命令维持 `--role all|api|agent` 角色(默认 all)与 `seed` 子命令,flag 与配置格式与重组前完全一致。单体 SHALL 通过组合各服务的导出装配包实现,SHALL NOT 复制服务 wiring。

#### Scenario: 单体行为与重组前一致
- **WHEN** 运行根 cmd/blowball serve(默认角色)
- **THEN** 在 server.port 注册完整 API+Agent 路由,行为与重组前 --role all 逐项一致

#### Scenario: 单体复用服务装配包
- **WHEN** 检查 cmd/blowball 的依赖
- **THEN** 它导入 services/apiserver 与 services/agent 的导出装配包,不直接导入服务 internal 包,不持有独立 wiring 副本

### Requirement: office-vers incorporated

office-vers 服务源码 SHALL 迁入 `services/officevers/`,import 路径 SHALL 重写为主 module 路径,其 openspec specs SHALL 原名义并入根 `openspec/specs/`。其运行时部署形态(URL、端口、容器)SHALL 不因迁入而改变。

#### Scenario: import 路径规范化
- **WHEN** 检查 services/officevers/ 内的 Go 导入语句
- **THEN** 不存在裸名 `office-version-service/...` 导入,全部使用 github.com/lush/blowball/services/officevers/... 路径

#### Scenario: openspec 并根
- **WHEN** 检查根 openspec/specs/
- **THEN** version-rollback 与 versioned-file-storage 两个 capability 存在且内容来自 office-vers 原 specs

#### Scenario: 迁入不改运行时行为
- **WHEN** 从迁入后的源码构建并启动 officevers 服务
- **THEN** 默认监听 :8088,行为与迁入前一致

### Requirement: Zero functional impact invariant

目录重组 SHALL NOT 改变运行时行为:主服务与 officevers 的二进制名、命令行 flag、配置文件格式与路径、监听端口、HTTP 契约 SHALL 与重组前一致。唯一允许的命名变化为 scheduler 二进制由 `scheduler` 规范化为 `blowball-scheduler`。

#### Scenario: 重组后全量测试绿
- **WHEN** 在仓库根执行 go vet ./... 与 go test -race ./...(含 test/integration)
- **THEN** 全部通过,无因目录重组引入的失败

#### Scenario: 镜像行为不变
- **WHEN** 用重组后的源码构建镜像并以原参数运行
- **THEN** 健康检查、路由、端口与重组前镜像一致

