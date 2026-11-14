# service-deployment Specification

## Purpose
定义部署物料布局：`deploy/<svc>/` 承载每个服务的 Dockerfile、Jenkinsfile 与独立部署片段（scheduler 的 systemd 单元），`deploy/compose/` 提供 `mysql-redis` 基础栈与共享数据卷的全栈 `all-in-one`；镜像统一以仓库根为构建上下文并注入版本信息，Jenkins 流水线按服务独立构建发布。
## Requirements
### Requirement: Per-service deploy directories

`deploy/` SHALL 按服务组织部署工件:每个服务一个独立目录(`deploy/apiserver/`、`deploy/agent/`、`deploy/scheduler/`、`deploy/officevers/`),内含该服务的 Dockerfile、Jenkinsfile 与独立部署片段(如 scheduler 的 systemd unit)。仓库根与各服务目录内 SHALL NOT 残留散落的 Dockerfile/compose 部署文件。

#### Scenario: 每服务部署目录完整
- **WHEN** 检查 deploy/ 目录
- **THEN** 四个服务目录各含 Dockerfile 与 Jenkinsfile,deploy/scheduler/ 含更新后的 blowball-scheduler.service

#### Scenario: 根部部署文件清理
- **WHEN** 检查仓库根与服务目录
- **THEN** 根目录无 Dockerfile/docker-compose.yaml,各服务源码目录内无部署文件

### Requirement: All-in-one compose

`deploy/compose/all-in-one.yaml` SHALL 一键拉起 apiserver、agent、scheduler、officevers、mysql、redis 全部组件:apiserver 暴露 server.port、agent 暴露 server.agent_port、officevers 暴露 :8088;apiserver 与 agent SHALL 挂载同一共享数据卷(工作区文件双向可见);mysql 初始化 SHALL 继续挂载根 migrations/。

#### Scenario: 全家桶一键拉起
- **WHEN** docker compose -f deploy/compose/all-in-one.yaml up -d
- **THEN** 全部组件就绪,四服务健康检查通过,apiserver 与 agent 在各自端口响应

#### Scenario: 共享数据卷贯通 api 与 agent
- **WHEN** 经 apiserver 上传工作区文件,随后由 agent 的 turn 通过 xizhi 工具读取该文件
- **THEN** agent 可见该文件;反向由 agent 写入的文件经 apiserver 的 workspace 文件接口可读

#### Scenario: 与单体行为对照一致
- **WHEN** 以相同配置分别运行全家桶 compose 与单体镜像(--role all 双端口形态)
- **THEN** HTTP 契约与端口暴露逐项一致

### Requirement: Image strategy per service

apiserver 镜像 SHALL 为薄镜像:静态二进制 + ca-certificates + tzdata,SHALL NOT 包含 bubblewrap、python、node、npm、git 等 Agent 运行时依赖。agent 镜像 SHALL 维持完整运行时依赖(bubblewrap、python3、py3-pip、nodejs、npm、git、bash、fd、ripgrep)。scheduler 与 officevers 镜像 SHALL 为最小 alpine 运行时。

#### Scenario: 薄 api 镜像
- **WHEN** 构建并检查 apiserver 镜像
- **THEN** 镜像内不含 python、node、bubblewrap,仅含静态二进制与基础运行时

#### Scenario: 厚 agent 镜像
- **WHEN** 构建并检查 agent 镜像
- **THEN** 镜像内含 bubblewrap、python3、nodejs、git、bash、fd、ripgrep,bash 执行器沙箱可用

### Requirement: Root build context

全部 Dockerfile SHALL 以仓库根为 build context 构建(`docker build -f deploy/<svc>/Dockerfile .`),`.dockerignore` SHALL 排除 data/、logs/、tmp/、bin/、.git 等运行时产物与本地产物(config.yaml)。

#### Scenario: 根 context 构建成功
- **WHEN** 对四个服务分别以仓库根 context 构建
- **THEN** 四个镜像全部构建成功,构建上下文不含 data/logs/tmp/bin 目录内容

### Requirement: Version injection on every image build

四个服务的镜像构建 SHALL 统一支持 `--build-arg VERSION/COMMIT/BUILD_TIME` 注入,经 ldflags 写入二进制;未提供时 SHALL 有安全默认值(dev/unknown),SHALL NOT 构建失败。

#### Scenario: 版本注入生效
- **WHEN** 以显式 VERSION/COMMIT/BUILD_TIME 构建任一服务镜像并运行 --version 类检查
- **THEN** 二进制报告注入的版本信息

#### Scenario: 缺省构建可用
- **WHEN** 不传版本 build-arg 构建任一服务镜像
- **THEN** 构建成功,版本信息为默认占位值

### Requirement: Per-service Jenkins pipeline

每个服务 SHALL 拥有自己的 Jenkinsfile,职责为:全量测试 → 构建镜像(根 context + 版本注入)→ 推送镜像。四份 Jenkinsfile SHALL 结构一致(仅服务名/镜像名参数化),registry 地址与凭据 SHALL 以参数/环境变量承载。同一 commit 触发多服务构建时 SHALL 使用同一镜像 tag。

#### Scenario: 单服务流水线
- **WHEN** Jenkins 按某服务的 Jenkinsfile 触发构建
- **THEN** 依次执行测试、镜像构建与推送,镜像 tag 携带该次 commit 标识

#### Scenario: 多服务同源构建
- **WHEN** 同一 commit 同时触发 apiserver 与 agent 流水线
- **THEN** 两个镜像使用相同 tag,保证共享 schema 的版本一致性

