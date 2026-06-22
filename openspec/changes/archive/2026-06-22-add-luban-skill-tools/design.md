## Context

Blowball 的 agent 通过 `read_skill` 工具读取 skill 文件，但 skill 文件位于 `data/{userID}/skills/`，而文件工具 xizhi 被限制在 `data/{userID}/workspace/` 内，无法跨越目录。模型因此尝试用 `xizhi_read_file("../skills/...")` 读取，被路径校验拒绝。同时，用户希望仅通过自然语言就能让 agent 从 GitHub 等 URL 安装整个 skill 集合（如 `obra/superpowers`），而当前没有安装机制。

当前 skill 发现逻辑为单层扫描 `{dir}/{name}/SKILL.md`，但 `superpowers` 这类集合仓库的真实 skill 位于 `{dir}/superpowers/skills/{name}/SKILL.md`，导致无法被发现。

## Goals / Non-Goals

**Goals:**
- 提供专门操作 skills 目录的 `luban_*` 内置工具：list、read、install。
- `luban_list_skills` / `luban_read_skill` 返回全局 + 用户 skills 的合并视图，用户覆盖全局。
- `luban_install_skill` 支持从 URL 整体安装 skill 集合到用户 `skills/` 目录。
- skill 发现支持递归扫描，识别嵌套在 skill 集合中的子 skills。
- system prompt 只注入全局 skills；用户 skills 由模型通过 `luban_*` 工具动态发现。
- system prompt 明确禁止用 `xizhi_*` 工具访问 skills 目录。

**Non-Goals:**
- 不修改 xizhi 工具的作用域或安全策略。
- 不提供 skill 的删除、更新版本管理或权限控制。
- 不修改 MySQL/Redis 中的 skill 存储；skills 仍只存在于文件系统。
- 不向前端暴露新的 HTTP API（luban 是纯 agent 工具）。

## Decisions

### 1. 用 `luban_*` 替代 `read_skill` 作为 agent 的 skill 读取入口
- **Rationale**: `read_skill` 名称与 xizhi 文件工具没有明显区分，模型容易混淆。`luban_*` 明确标识为 skill 管理工具，配合 system prompt 禁止 xizhi 访问 skills，可减少误用。
- **Alternative**: 保留 `read_skill` 并增强描述。但无法解决模型用 xizhi 读 skills 的根本问题，因为 xizhi 工具仍然在 agent 的工具列表里。

### 2. system prompt 只注入全局 skills，不预注入用户 skills
- **Rationale**: 用户 skills 是动态安装的，启动时无法预知；由模型在运行时通过 `luban_list_skills` 发现更符合用户"通过工具查找"的诉求。
- **Alternative**: 同时注入全局和用户 skills。但用户目录可能为空或动态变化，且用户明确希望"用户自己的通过模型去调用 skills 工具发现"。

### 3. `luban_list_skills` 扫描全局 + 用户，返回合并视图
- **Rationale**: 作为保险查询工具，返回完整视图最符合直觉；"全局且用户都找不到就是没有"也支持这一语义。
- **Implementation**: 复用 `skill.Loader.List(userID)`，但在注入 system prompt 时单独调用只扫描全局的方法（如 `Loader.ListGlobal()` 或传入空 userID）。

### 4. `luban_install_skill` 优先使用 `git clone`
- **Rationale**: GitHub repo 是最常见的 skill 集合来源，`git clone --depth 1` 能保留目录结构，便于递归发现子 skills。
- **Alternative**: 下载 tar/zip 归档。需要额外处理解压，但减少 git 依赖。考虑到 skill 集合可能引用同仓库其他文件，git clone 更健壮。
- **Fallback**: 如果 URL 以 `.md` 结尾或明确指向单个文件，则直接下载到 `{name}/SKILL.md`。

### 5. skill name 从 URL 和 frontmatter 双重推断
- **Rationale**: 安装时需要确定目标目录名。URL 路径最后一段作为默认值，安装后解析 SKILL.md frontmatter 中的 `name` 做最终校验/重命名。
- **Example**: `https://github.com/obra/superpowers` → 默认 `superpowers`；如果 frontmatter 中的 name 不同，以 frontmatter 为准或报错。

### 6. 递归发现 skill 时以 `SKILL.md` 的存在为判定标准
- **Rationale**: 避免把普通子目录误判为 skill。只要某目录下直接存在 `SKILL.md` 且 frontmatter 有效，就视为一个 skill。
- **Trade-off**: 可能把非 skill 的 `SKILL.md` 识别进来，但 frontmatter 中的 `name`/`description` 校验会过滤掉大部分噪音。

## Risks / Trade-offs

- **[Risk] `git clone` 引入外部依赖和网络安全风险** → **Mitigation**: 只支持 `https://` URL；限制 clone 深度为 1；对下载内容大小做上限；运行 git 命令前校验 URL scheme 和主机白名单（可选）。
- **[Risk] 递归扫描导致性能问题** → **Mitigation**: 限制最大递归深度（如 4 层）或扫描目录总数上限；全局 skills 目录通常很小，用户目录也受安装工具控制。
- **[Risk] 同名 skill 覆盖策略引发意外** → **Mitigation**: `luban_install_skill` 默认覆盖已存在的同名 skill，并在返回结果中说明；工具描述中明确说明此行为。
- **[Risk] 模型仍然尝试用 xizhi 读 skills** → **Mitigation**: system prompt 显式禁止；同时在工具描述里强调 "Use luban_read_skill, not xizhi_read_file"；如仍出现，可进一步强化提示词或监控。
- **[Risk] `read_skill` 被弃用后已有集成中断** → **Mitigation**: 保留 `read_skill` 的注册但不在 agent tools 列表中使用；config.yaml 迁移文档中说明替换方式。

## Migration Plan

1. 新增 `internal/tool/luban/` 包并注册工具。
2. 修改 `internal/tool/skill/skill.go` 支持递归发现和全局/用户分离查询。
3. 修改 `internal/prompt/render.go` 更新 Skills 段落文案。
4. 修改 `internal/agent/orchestrator.go` 只注入全局 skills，并把 luban 工具复制到 per-request registry。
5. 修改 `cmd/server/main.go` 注册 luban 工具并调整启用检测逻辑。
6. 更新 `config.yaml`：将 agent `tools` 中的 `read_skill` 替换为三个 luban 工具；在 `agent.skills` 中声明需要注入的全局 skills。
7. 运行 `make test` 和集成测试，验证 tool registry 注册、system prompt 渲染、skill 发现逻辑。

## Open Questions

- 是否需要限制 `luban_install_skill` 只能安装来自特定域名（如 github.com）的 URL？
- 是否需要为 luban 工具添加独立的配置节（如超时、大小限制、递归深度）？
- `read_skill` 是否在本次变更中彻底移除注册，还是保留作为兼容入口？
