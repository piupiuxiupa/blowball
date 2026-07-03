# deletion-archive Specification

## Purpose

定义会话删除前的逐列（verbatim）归档能力：为每张现有数据表（`users` / `sessions` / `titles` / `messages`）维护一张 `*_deleted` 镜像表，在清除源数据之前将源行归档到对应镜像表，并附加 `deleted_at` 与 `deletion_id` 审计列；归档与清除在同一事务内原子完成；`users_deleted` 作为未来用户删除能力的脚手架预留，当前不写入。

## Requirements

### Requirement: Deletion archive mirror tables
系统 SHALL 为每张现有数据表（`users` / `sessions` / `titles` / `messages`）维护一张 `*_deleted` 镜像表，在删除源数据之前将源行逐列（verbatim）拷贝到对应镜像表，并附加 `deleted_at`（删除时间）与 `deletion_id`（标识同一次删除操作的 UUID）两个审计列。镜像表不带外键约束，`messages_deleted.id` 为普通 `BIGINT`（保留源 id 值，非 AUTO_INCREMENT）。

#### Scenario: 镜像表结构与源表一致
- **WHEN** 系统创建 `users_deleted` / `sessions_deleted` / `titles_deleted` / `messages_deleted`
- **THEN** 每张镜像表包含源表的全部列（类型一致），外加 `deleted_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP` 与 `deletion_id CHAR(36) NOT NULL`；镜像表无外键约束；`messages_deleted.id` 为普通 `BIGINT` 并保留源 `messages.id` 值

#### Scenario: 删除前原样归档
- **WHEN** 一次删除操作清除某个 session 及其 titles/messages
- **THEN** 系统在清除源行之前，把 `sessions`、`titles`、`messages` 的源行逐列写入对应 `*_deleted` 表，且同一次删除产生的全部镜像行共享同一个 `deletion_id` 与同一次 `deleted_at`

#### Scenario: 归档与清除原子
- **WHEN** 归档或清除的任一步骤失败
- **THEN** 整个删除操作回滚：源表数据保持完好，且不出现任何部分归档

#### Scenario: 镜像表独立存活
- **WHEN** 源用户或会话被删除
- **THEN** 对应 `*_deleted` 镜像行不被级联删除（镜像表无外键），予以保留以供审计或恢复

### Requirement: users_deleted is scaffolding
系统 SHALL 创建 `users_deleted` 镜像表作为后续用户删除能力的脚手架预留；当前变更不提供用户删除接口，也不向 `users_deleted` 写入任何数据。

#### Scenario: 当前无写入路径
- **WHEN** 本次变更范围内的会话删除或文件删除发生
- **THEN** 系统不向 `users_deleted` 写入数据（该表存在但为空，留待未来用户删除能力启用）
