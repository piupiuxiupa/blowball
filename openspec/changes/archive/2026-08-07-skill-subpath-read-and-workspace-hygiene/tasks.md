## 1. Config 基础

- [x] 1.1 在 `internal/config/config.go` 的 `XizhiConfig` 新增 `Delete XizhiToolConfig `yaml:"delete"`` 字段（参照既有 `ListFiles`/`Tree`/`GlobFiles`）
- [x] 1.2 确认 `XizhiConfig` 解析/默认行为与既有开关一致（`tools.xizhi.delete.enabled` 默认 false，未配置即不注册）

## 2. luban_read_skill 按路径读取技能子文档

- [x] 2.1 在 `internal/tool/skill/skill.go` 的 `Loader` 新增按"技能名 + 技能内相对路径"读取 `.md` 文件的方法（如 `ReadPath(name, relPath, userID)`）：先按 name 查到匹配 skill 得到 `Skill.Path`，技能目录根 = `filepath.Dir(Skill.Path)`
- [x] 2.2 在 `internal/tool/skill/`（或 luban 包内）实现"限制在技能目录根内"的路径校验：拒绝绝对路径、`filepath.Clean` 后逃逸技能目录根的 `..`、`filepath.EvalSymlinks` 解析后落在根外的符号链接；仅放行扩展名 `.md`；复用 `maxSize` 大小上限
- [x] 2.3 读取内容经 `parseFrontmatter` 处理（有 frontmatter 剥离、无则原样）；文件不存在返回 "file not found"
- [x] 2.4 在 `internal/tool/luban/register.go` 的 `registerReadSkill` schema 与执行体新增可选 `path` 参数；`path` 省略时走既有 `Read(name)` 读 `SKILL.md`（向后兼容），提供时走新的按路径读取
- [x] 2.5 更新 `luban_read_skill` 工具描述：声明省略 `path` 读 `SKILL.md`、提供 `path` 读技能目录树内 `.md`（相对路径、仅 `.md`、限制在技能目录根内）
- [x] 2.6 单元测试覆盖（`internal/tool/skill/`、`internal/tool/luban/`）：读嵌套子文档、frontmatter 剥离、绝对路径拒绝、`..` 逃逸拒绝、符号链接逃逸拒绝、非 `.md` 拒绝、超限拒绝、缺失文件错误、`name` 仍为标识符（路径形式 name 被拒）、`path` 省略向后兼容

## 3. xizhi_delete 工具

- [x] 3.1 在 `internal/tool/xizhi/delete.go` 实现删除：复用 `validatePath`（拒绝绝对路径/`..`/符号链接逃逸）与保留目录 `.blowball` 拒绝规则；文件与目录均支持，目录递归删除；不存在视为幂等成功；返回 `{path, deleted, type}`（`type` ∈ `file`/`directory`/`none`）
- [x] 3.2 在 `internal/tool/xizhi/register.go` 新增 `NameDeleteFile = "xizhi_delete"` 常量、`schemaDelete`（必填 `path`），并在 `RegisterAll` 中按 `cfg.Delete.Enabled` 条件注册（与 `list_files`/`tree`/`glob` 一致）
- [x] 3.3 `xizhi_delete` 工具描述声明结果结构 `{path, deleted, type}`、目录递归删除、缺失幂等成功、`path` 须相对工作空间根、并指明其为 `tmp/` 草稿清理的删除原语
- [x] 3.4 单元测试覆盖（`internal/tool/xizhi/`）：删文件、删目录递归、缺失幂等、绝对路径拒绝、`..` 越界拒绝、符号链接逃逸拒绝、`.blowball` 保留目录拒绝、错误含相对路径示例、`enabled=false` 时不注册

## 4. 系统提示词工作空间输出规范与 tmp 清理指引

- [x] 4.1 在 `internal/prompt/render.go` 的 `renderWorkspaceConvention()` 扩充文案：临时产物入 `tmp/`、交付物入工作空间并按主题归组、相关文件同目录、草稿使命结束后优先用 `xizhi_delete`（不可用时 `bash rm`）及时清理 `tmp/`、禁止把 `tmp/` 路径作为交付物路径交给用户；与既有"相对路径/`/tmp`→`./tmp/`"指引并存
- [x] 4.2 单元测试覆盖（`internal/prompt/`）：渲染输出包含上述四点指引、与既有约定段共存

## 5. 配置示例与接线

- [x] 5.1 在 `config.example.yaml` 的 `tools.xizhi` 段补充 `delete` 开关示例（默认注释/关闭），说明启用后需在 Agent `tools` 列表加入 `xizhi_delete`
- [x] 5.2 在 `config.example.yaml` 的 Chongzhi `tools` 列表加入 `xizhi_delete`（与既有 `xizhi_*` 并列）

## 6. 验证

- [x] 6.1 `make lint` 通过
- [x] 6.2 `make test`（含 `go test ./internal/tool/... ./internal/prompt/...`）通过
- [x] 6.3 `openspec validate skill-subpath-read-and-workspace-hygiene --strict` 通过
