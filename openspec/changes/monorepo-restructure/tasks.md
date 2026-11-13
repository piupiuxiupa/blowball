# Tasks: monorepo-restructure

按 P1(纯重组)→ P2(拆服务 + 部署工件)分阶段,每组完成即独立可验收。P3 优化不在本清单。

## 1. P1 准备:包归属盘点

- [x] 1.1 生成导入关系清单:对根 `internal/` 每个包,统计被哪些服务(cmd/blowball 各角色 wiring、services/scheduler 生产代码)导入,产出 pkg / services/apiserver/internal / services/agent/internal 三归属划分表(产出物:division-table.md)
- [x] 1.2 按划分表标记例外:仅测试代码跨服务导入的包不进 pkg(如 scheduler 测试导入 turn/store);`internal/pkg/*` 全部归 pkg;`api/gen/go`、`migrations/` 留根,在划分表中显式记录

## 2. P1:目录重组(单 module,git mv + import 重写)

- [x] 2.1 `git mv` 迁移共享包至根 `pkg/`(含原 `internal/pkg/` 并入),重写全部 import 路径,`go build ./...` 通过
- [x] 2.2 主服务包按归属迁入 `pkg/`(任务 1.1 数据判定:全部主服务包为跨角色/跨服务共享,无服务专属包;`services/apiserver/`、`services/agent/` 目录在 P2 任务 5.1/5.2 随 wireAPI/wireAgent 装配拆分创建),`cmd/blowball` 保留在根、import 更新后全量构建通过
- [x] 2.3 `services/scheduler/` 内部结构对齐新布局(cmd 入口命名规范),import 重写,构建通过
- [x] 2.4 迁移 `deploy/redis-ha/` 原样保留并确认引用文档路径
- [x] 2.5 验证门槛:`go vet ./... && go test -race ./...`(含 test/integration)全绿;`make build`/`make run`/`make run-api`/`make run-agent`/`make build-scheduler` 目标更新后全部可用

## 3. P1:office-vers 纳入

- [x] 3.1 将 office-vers 源码(cmd/、internal/、docs/、scripts/)复制入 `services/officevers/`,import 路径重写为 `github.com/lush/blowball/services/officevers/...`(9 处),删除其 go.mod/go.sum,依赖并入根 go.mod,`go mod tidy` + 构建通过
- [x] 3.2 office-vers 的 openspec specs(version-rollback、versioned-file-storage)原名义并入根 `openspec/specs/`,处理其 changes/archive 残留;根 `openspec list` 正常
- [x] 3.3 在 services/officevers/README(或根 README 相应章节)注明来源仓库与截至 commit(99ef6c2)

## 4. P1:deploy/ 骨架与根部部署文件迁移

- [x] 4.1 建 `deploy/{apiserver,agent,scheduler,officevers,compose}` 目录;根 Dockerfile 拆迁:暂以现有单镜像 Dockerfile 迁至 `deploy/agent/`(厚镜像,内容暂不变)并为其余服务放置占位 Dockerfile(与现行为等价),根 docker-compose.yaml 内容并入 `deploy/compose/`(暂为 mysql+redis),根部部署文件删除
- [x] 4.2 `.dockerignore` 补齐 data/、logs/、tmp/、bin/、.git、config.yaml;验证四个 `docker build -f deploy/<svc>/Dockerfile .` 根 context 构建成功
- [x] 4.3 全量验证 + P1 收尾提交;更新 CLAUDE.md、AGENTS.md、README(含 openapi.yaml 同步到 blowball-frontend 的路径指引)、docs/ 与 openspec specs 中的路径引用

## 5. P2:服务装配拆分与独立二进制

- [ ] 5.1 将 serve.go 集中 wiring 按角色拆入 `services/apiserver/server` 与 `services/agent/server` 导出装配包(构造依赖 + 路由注册 + 生命周期),根 cmd/blowball 改为纯组合,行为不变(以现集成测试为对照)
- [ ] 5.2 新增 `cmd/blowball-api`(固定 api 角色 + seed 子命令)与 `cmd/blowball-agent`(固定 agent 角色)薄 main:接受 -f/-d,拒绝 --role(非零退出);`make build-api`/`make build-agent` 目标
- [ ] 5.3 scheduler 入口更名产出 `blowball-scheduler`,更新 `deploy/scheduler/blowball-scheduler.service` 与 Makefile
- [ ] 5.4 验证门槛:全量测试绿;blowball-api/blowball-agent 与单体 --role api/--role agent 的路由集合、端口、错误行为逐项对照一致;blowball-api seed 与单体 seed 行为一致

## 6. P2:镜像定型与全家桶 compose

- [ ] 6.1 apiserver Dockerfile 瘦身定型:静态二进制 + ca-certificates + tzdata(不含 python/node/bubblewrap/git);agent Dockerfile 维持厚依赖并从 `services/` 新路径构建;scheduler、officevers Dockerfile 定型(alpine 最小运行时)
- [ ] 6.2 四个 Dockerfile 统一接入 `--build-arg VERSION/COMMIT/BUILD_TIME` → ldflags(含安全默认值)
- [ ] 6.3 编写 `deploy/compose/all-in-one.yaml`:四服务 + mysql(migrations 挂载)+ redis;apiserver 与 agent 挂同一共享数据卷;端口按角色暴露
- [ ] 6.4 全家桶冒烟:一键拉起四服务健康;验证共享卷链路(apiserver 上传 → agent turn 内 xizhi 读取/写入 → apiserver 读回、artifact 版本解析);与单体形态 HTTP 契约对照一致

## 7. P2:Jenkinsfile 与收尾

- [ ] 7.1 四份结构一致的 Jenkinsfile(测试 → 根 context 构建 → 版本注入 → 推送),服务名/镜像名/registry/凭据参数化,同 commit 同 tag
- [ ] 7.2 更新 docs/(scheduled-tasks.md 等含路径/二进制名引用)与 CLAUDE.md/AGENTS.md 至终态布局;迁移说明记录 scheduler 更名与生产切换路径(单体镜像可继续过渡)
- [ ] 7.3 终验:全量测试 + 四镜像构建 + 全家桶 compose 冒烟通过;`openspec` specs 与实现一致,准备归档
