## Context

现有三层存储：Redis（热，TTL）→ FS（暖，`data/{userID}/sessions/{id}.json`，存的是该会话**完整消息列表** JSON）→ MySQL（真源，`sessions`/`titles`/`messages` 之间 `ON DELETE CASCADE`）。

现有删除原语零散、未接通：
- `fs.Store.DeleteSession` 已存在且已在 `FSStore` 接口（幂等，缺文件返回 nil）。
- `redis.ClearMessages` / `redis.DelSessionCache` 已存在但**未在 `RedisStore` 接口暴露**，仅测试调用。
- **MySQL 无任何 delete 方法**；`MySQLStore` 接口无 delete 签名。

读路径安全前提（已验证）：`GetSessionMessages` / `SendMessage` / `ListSessions` 都先调 `GetSessionByID` 做所有权校验，硬删除会话后返回 404，**不会触达 Redis/FS**——故 Redis 留待 TTL 过期在当前代码下安全。

文件只存在于 FS（`data/{userID}/workspace/`），MySQL 无任何文件表；上传上限硬编码 `50 MiB`（`main.go:66`），xizhi 写/改文件无上限，故工作空间内可能有较大二进制文件。

用户已拍板的关键决定：四张现有表各建一张 `*_deleted` 原样保留；Redis 不处理；FS 会话文件清理；文件删除支持目录（递归）；永久不适配 Doris（已删 `007_doris_schema.sql`）。

## Goals / Non-Goals

**Goals:**

- 会话删除：归档 → 清除 → 清理 FS，单个事务保证原子。
- 四张 `*_deleted` 镜像表原样保留删除数据 + 审计列（`deleted_at` / `deletion_id`）。
- 文件/目录递归删除，路径越界拒绝（复用 `xizhi.ValidatePath`）。
- 仅后端实现 + 更新 `api/openapi.yaml`。

**Non-Goals:**

- 不做用户删除接口（`users_deleted` 本次仅建表作为脚手架，不写入）。
- 不做文件删除归档（文件无源表）。
- 不做归档数据的恢复/导出接口（仅保留）。
- 不做归档保留期清理任务（永久保留）。
- 不适配 Doris。
- 不改前端、不执行 `npm run generate-api`。

## Decisions

### 1. 每张现有表建独立 `*_deleted` 镜像，而非单一通用 JSON 归档表

- **Rationale**：用户明确要求“四张表每张建一个 xxx_deleted，原样保留”。逐行镜像用服务端 `INSERT … SELECT` 拷贝，**天然规避单 blob 的 MEDIUMTEXT 溢出**（单会话消息量无上限）；归档可按表用 SQL 查询/审计/恢复。
- **Alternative**：单一 `deletion_records(entity_type, payload LONGTEXT JSON)`。单 blob 仍有 LONGTEXT 上限风险（无界消息 × 每条 MEDIUMTEXT），且不可按列查询。

### 2. 镜像表 = 源列原样 + `deleted_at` + `deletion_id`，无外键；`messages_deleted.id` 为普通 BIGINT

- **Rationale**：“原样保留”= 源列逐字；`deleted_at` 不可省（否则归档无时间维度）；`deletion_id`（每次删除操作一个 UUID）分组同一次删除写入的全部行；**无 FK** 使归档独立存活（未来删用户不会连带级联删归档）；`messages.id` 是 AUTO_INCREMENT，镜像须保留原值，故改为普通 `BIGINT`（源 id 不会复用，可作 PK）。
- **Alternative**：严格只复制源列。但缺 `deleted_at` 无法审计。

### 3. 归档与清除在单个 MySQL 事务内完成（`INSERT…SELECT` ×3 + `DELETE sessions`）

- **Rationale**：级联删除不可逆，必须先归档；事务保证“全归档 + 全清除”原子，任一步失败回滚（live 数据完好、无半归档）。消息 `content`（MEDIUMTEXT）服务端拷贝，**不进 Go 内存**，规避无界 size 的内存压力。
- **Alternative**：先 `SELECT` 到 Go 再 `INSERT`。受消息总量/单条 MEDIUMTEXT 内存压力，且多 round-trip。

### 4. Redis 不主动清理（依赖 TTL）

- **Rationale**：用户决定；侦察验证安全——所有读路径先过 `GetSessionByID`，硬删除后返回 404，stale `msgs:{id}` 当前不可达。TTL 到期自动消失。
- **Trade-off / 可选加固**：`msgs:{id}` 的“不可达”是**隐式**不变量；未来若出现绕过所有权校验的后台任务/管理端点，会从 Redis 复活已删数据。`redis.ClearMessages` 已存在且幂等，近零成本即可防御性清理。本次按用户决定跳过，记为后续可选加固。

### 5. FS 会话 JSON 删除

- **Rationale**：用户决定“清理一下”；`fs.DeleteSession` 已存在幂等；`ReadSession` 缺文件返回 `(nil,nil)`，删除不破坏读链（FS miss → MySQL 命中或 404）。

### 6. 文件删除：递归 `os.RemoveAll`，无 DB 归档

- **Rationale**：用户要求可删目录；`xizhi.ValidatePath` 已在入口拒绝绝对路径 / `..` / 符号链接逃逸，**验证通过的目录其内容必在 workspace 内**，递归删除安全（残余风险仅用户误删，属其自身工作空间）。文件无 MySQL 源表，“原样保留”无对象可镜像。
- **Alternative**：仅删文件、拒绝目录。但用户明确要求可删目录。

### 7. 迁移编号 008（单文件含 4 表）

- **Rationale**：`007` 曾是 Doris（已删），不复用以免与 git 历史混淆；4 表同属一个 feature 放一个文件，编号一步到位。

### 8. 所有权校验返回 404 而非 403

- **Rationale**：与 `GetSessionMessages` 现有约定一致，避免泄露会话存在性。

## Risks / Trade-offs

- **[Risk] 事务内大消息归档耗时长/持锁** → **Mitigation**：`INSERT…SELECT` 服务端拷贝不进 Go 内存；按 `session_id` 索引限制扫描范围；事务粒度为单会话，可控。
- **[Risk] 镜像表无 FK 导致归档与用户/会话“孤立”** → **Mitigation**：这是预期行为（归档独立存活）；按 `user_id` / `session_id` 建索引便于查询。
- **[Risk] 递归删目录误删大量文件** → **Mitigation**：`ValidatePath` 保证不逃逸；限定调用者本人工作空间；未来可加 `.trash` 回收站（见 Open Questions）。
- **[Risk] stale Redis 复活已删数据**（见 Decision 4）→ **Mitigation**：当前安全；可选 `ClearMessages` 加固。
- **[Risk] `users_deleted` 建而不用造成困惑** → **Mitigation**：proposal/design 明确标注为脚手架，本次不写入。

## Migration Plan

1. 新增迁移 `migrations/008_deletion_archive.sql`（4 张镜像表）。
2. `internal/store/mysql/` 新增 `DeleteSession`（事务 archive+purge）+ 镜像表 SQL。
3. `internal/service/deps.go` 扩展 `MySQLStore` 接口；`internal/service/session.go` 新增 `SessionService.DeleteSession`。
4. `internal/handler/session.go` 新增 `DeleteSession`；`internal/handler/workspace.go` 新增 `Delete`。
5. `internal/handler/router.go` 扩展 `RouteDeps` + 注册 DELETE 路由；`cmd/server/main.go` 装配。
6. 更新 6 个测试 fake + 新增单测/集成测试。
7. 更新 `api/openapi.yaml`；修正 `CLAUDE.md:155`。
8. `make test` / `make lint`。

## Open Questions

- 归档数据是否需要保留期/定期清理任务？（当前永久保留）
- 是否需要“恢复已删除会话/消息”接口？（当前仅保留不恢复）
- 是否对文件删除也加 `.trash` 回收站？（当前直接物理删除）
