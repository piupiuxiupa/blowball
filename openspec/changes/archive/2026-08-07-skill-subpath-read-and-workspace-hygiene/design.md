## Context

两个独立但相关的 Agent 文件操作痛点：

1. **技能子文档不可读。** `luban_read_skill`（`internal/tool/luban/read.go` + `internal/tool/skill/skill.go`）当前只能读取与 skill 名匹配的那个 `SKILL.md`。技能发现（`Loader.discover`）本身已递归到深度 5，能找到任意深度的 `SKILL.md`，但读取被锁定在该单一文件——技能目录树内的示例、参考等关联 `.md` 文件对 Agent 不可见。

2. **工作空间输出无规范、`tmp/` 无清理。** 现状 `internal/prompt/render.go` 的 `renderWorkspaceConvention()` 只说明了"`xizhi_*` 用相对路径""沙箱 `/tmp` 映射到 `./tmp/`"，未指导模型把临时产物与交付物分开、未要求相关文件归组、`tmp/` 草稿目录也无人清理越积越多。更根本地，Xizhi 工具集（`internal/tool/xizhi/register.go`）只有 read/write/modify/list/tree/glob，**缺少删除原语**——Agent 唯一能删文件的途径是 `bash rm`，而 `bash` 仅在 Linux + bubblewrap + 配置启用时存在，且其工具描述主动劝阻用 bash 做文件操作、`rm` 还是审计告警关键字。

## Goals / Non-Goals

**Goals:**
- 让 `luban_read_skill` 能按相对路径读取技能目录树内任意 `.md` 文件（限制在技能目录根内、仅 `.md`）。
- 新增 `xizhi_delete` 工具，使 Agent 在任何部署（含无 `bash` 的环境）都具备删除工作空间文件/目录的能力。
- 通过系统提示词约定，指导模型判断：临时产物入 `tmp/`、交付物入工作空间并归组、草稿使命结束后及时清理 `tmp/`、不得把 `tmp/` 路径作为交付物。

**Non-Goals:**
- 不新增其它 luban 工具（不提供"列出技能内文件"的发现工具）——本变更只优化 `luban_read_skill` 本身。
- 不做代码级的 `tmp/` 清理（无后台 sweeper、无 turn-start `os.RemoveAll`）——清理完全由提示词引导模型用 `xizhi_delete` 完成。
- 不为 `luban_read_skill` 支持非 `.md` 文件，不支持跨技能目录（`..` 逃逸出技能目录根）的读取。
- 不改 API 契约、不改 DB schema、不做破坏性变更（`path` 可选、`xizhi_delete` 受开关控制）。

## Decisions

### 决策 1：在 `luban_read_skill` 上新增可选 `path`，不新建工具
用户明确要求"只优化 `luban_read_skill`、不添加其它 luban 工具"。`path` 可选，省略时读 `SKILL.md`（向后兼容），提供时读技能树内 `.md`。
- *备选*：新建 `luban_read_skill_file` 专用工具。**否决**——与用户指令相悖，且"一个工具、一个心智模型"更清晰。

### 决策 2：`path` 的限制根 = 匹配到的 `SKILL.md` 所在目录；阻断 `..` 逃逸
`discover` 已把匹配 skill 的绝对路径存入 `Skill.Path`，技能目录根 = `filepath.Dir(Skill.Path)`。`path` 相对该根解析，经 `filepath.Clean` + `EvalSymlinks` 后必须仍在根内，否则拒绝。即"只能读本技能自己的文件树，不能读其兄弟"。
- *备选*：允许 `..` 攀升到技能集合的共享文档目录。**否决**——引入跨目录遍历问题，破坏直观隔离，且现有 `validateSkillName` 对 `name` 的标识符约束已确立"一个 name = 一个目录"的心智。
- 复用 `internal/tool/xizhi/validate.go` 的 confine-to-root 模式，仅把根从 `workspaceRoot` 换成技能目录根。`name` 的既有标识符校验（拒绝分隔符/`..`/绝对路径）保持不变，**只有 `path` 是文件相对路径**。

### 决策 3：`path` 仅允许 `.md`；frontmatter 有则剥、无则原样
用户明确"文件是否是 md 去阅读"。扩展名非 `.md` 返回错误。frontmatter 复用现有 `parseFrontmatter`（无 `---` 前缀时原样返回 body）。大小复用与 `SKILL.md` 相同的上限（默认 500KB）。

### 决策 4：新增 `xizhi_delete` 工具（而非仅靠 `bash rm`）
提示词引导模型自行清理（用户要求"用提示词指导模型判断是否清理，不需要额外修改代码"——指无需后台清理代码），但模型需要一个**在所有部署都可用**的删除原语。
- *备选 A*：仅依赖 `bash rm`。**否决**——`bash` 仅 Linux + 配置启用时存在；macOS 开发与未配置 `bash` 的部署上模型**完全无法删文件**，提示词指令沦为空操作。且 `bash` 工具描述主动劝阻文件操作、`rm` 是审计告警关键字。
- *备选 B*：用 `xizhi_write_file` 覆写空内容。**否决**——只清空不删除，`tmp/` 仍残留零字节文件，非真正清理。
- `xizhi_delete` 与 read/write/modify 一并始终注册（前三个工具为向后兼容总是注册），但参照 `list_files`/`tree`/`glob` 的开关模式，由 `tools.xizhi.delete.enabled` 控制。删除目录时递归删除。沿用 `validatePath`（拒绝绝对路径/`..`/符号链接逃逸）与保留目录 `.blowball` 拒绝规则。

### 决策 5：`xizhi_delete` 复用既有路径校验与保留目录规则，无需新校验
`internal/tool/xizhi/validate.go` 的 `validatePath` 与"保留目录 `.blowball` 拒绝"已覆盖删除所需的全部安全约束（per-user workspace 作用域、相对路径强制、符号链接/`..`/绝对路径拒绝、`.blowball` 拒绝）。删除只是增加一个写语义上的破坏性操作，路径安全模型与 `xizhi_write_file` 覆写一致。

### 决策 6：清理逻辑由提示词承载，约定写入 `renderWorkspaceConvention()`
用户指定提示词写在 `internal/prompt/render.go`。约定段（与既有"相对路径/`/tmp` 映射"并存）明确四点：临时产物→`tmp/`、交付物→工作空间并按主题归组、相关文件同目录、草稿使命结束后用 `xizhi_delete`（不可用时 `bash rm`）及时清理、禁止把 `tmp/` 路径作为交付物。全局生效，对非文件型 Agent 无害。

## Risks / Trade-offs

- **[模型把交付物误判为临时、塞进 `tmp/` 并删除]** → 约定提示词强调交付物入工作空间、`tmp/` 明确声明为临时且禁作交付物路径；属软约束（提示词引导，非硬强制），与"用提示词判断"的用户意图一致。
- **[提示词清理是 best-effort，模型可能忘记清理]** → 用户明确接受此取舍；`tmp/` 堆积无害（受工作空间约束），无正确性影响。
- **[技能子文档无发现手段——`SKILL.md` 不引用则模型不知路径]** → 已接受为 Non-Goal（不新增列出工具）；自描述其子文档的技能可用，发现能力留待未来变更。
- **[`xizhi_delete` 若模型过于激进可能删除用户真实文件]** → 受既有路径作用域约束（per-user workspace、`.blowball` 拒绝），信任模型与 `xizhi_write_file` 覆写同级；删除为常规工作空间维护操作。
- **[`rm`/`bash` 兜底会触发审计告警关键字日志]** → 可接受，非阻塞；主路径优先 `xizhi_delete` 以避免告警。

## Migration Plan

- 纯新增 + 向后兼容，无需停机迁移。
- `luban_read_skill` 的 `path` 可选，既有调用（仅传 `name`）行为不变。
- `xizhi_delete` 受 `tools.xizhi.delete.enabled` 控制，默认关闭时与现状完全一致；部署需在 `config.yaml` 的 `tools.xizhi.delete.enabled: true` 并在 Agent（如 Chongzhi）`tools` 列表加入 `xizhi_delete` 后才生效。
- 提示词约定段为纯渲染文案变更，下一轮对话即生效，无持久化影响。
- 回滚：还原 `internal/prompt/render.go`、移除 `xizhi_delete` 注册与 `path` 参数即可，无数据需迁移。

## Open Questions

探索阶段已全部厘清，无遗留悬而未决项。实现期唯一的小裁量是 `renderWorkspaceConvention()` 的确切文案措辞（属实现细节，不改变规格语义）。
