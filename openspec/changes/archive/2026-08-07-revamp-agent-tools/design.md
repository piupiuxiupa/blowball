## Context

Blowball 的 agent 工具层散布在四个包：`internal/tool/xizhi`（工作区文件操作）、`internal/tool/executor`（bwrap 沙箱执行）、`internal/tool/luban`（技能管理）、`internal/tool/webfetch`。本次改动横跨前三者，并触及若干下游 spec，属于跨模块的工具层梳理。

当前状态与约束：

- **xizhi 只有按名搜索**（`xizhi_glob_files`，基于 `doublestar`），缺按内容搜索；定位符号/引用只能逐文件 `xizhi_read_file` 肉眼扫。
- **luban_read_skill 硬限 `.md`**：限制写在 `skill.validateSkillSubPath`（`if ext != ".md" { reject }`）与工具描述两处。但 `luban_install_skill` 已支持 `git clone` 整仓（技能 = 包），仓内的脚本/模板/配置等文本资产读不出来。
- **executor 三个工具高度重叠**：`python`/`pip_install` 本质是 `bash` 的特化。关键耦合是 `pip_install` 写 `/workspace/.pip`、`python` 经 `PYTHONPATH` 读它——而 `bwrap.go` 里 `PYTHONPATH=/workspace/.pip` 是**对每个沙箱无条件注入**的（不区分 bash/python/pip），所以砍掉专用工具后这个桥仍对 bash 生效。
- **bash 描述已有部分让位引导**（`cat`/`echo`/`find`/`grep` → `xizhi_*`），但名单不全；且当前**无命令入参拦截**，只有 `dangerousCommandPattern`（`rm`/`curl`/`wget`/`sudo`/`sshd`）的 warn-only audit。
- 配置用非严格 `yaml.Unmarshal`，删除 struct 字段后老配置残留键被静默忽略，不致启动失败。

## Goals / Non-Goals

**Goals:**
- 新增 `xizhi_grep`：以 ripgrep 体验（正则 + 文件名过滤 + 行号 + 上下文行 + 大小写开关）按内容搜索工作区，省 token、可定位。
- 让 `luban_read_skill` 能读 skill 目录树内任意文本资产，二进制明确拒绝。
- 精简执行器为单一 `bash`，python/pip 统一经 bash 调用，且 pip-via-bash 的包仍可被 bash 内 python3 导入（PYTHONPATH 桥保留）。
- 用纯提示词（非代码）强化「专用工具优先」引导，并把让位名单补全。
- 保持现有安全姿态：grep 走 `validatePath`（拒 `.blowball`、不跟 symlink）；dangerousCommandPattern warn audit 不变。

**Non-Goals:**
- **不做 bash 命令入参的代码拦截**（无论正则硬拦还是 AST 解析）——见决策 D4 的论证。
- 不引入新的第三方库（grep 用标准库 `regexp`/`filepath`/`bufio`）。
- 不改变 `pip_install` 曾经的「窄网络出口」安全模型——网络放开的取舍已在本次明确接受（见 D3）。
- 不动 Landlock / bwrap 沙箱的目录策略、不动 `PYTHONPATH` 注入的实现、不动 `.pip` 落盘位置。
- 不重构 xizhi 既有的 read/write/modify/list/tree/glob/delete 工具。

## Decisions

### D1. grep 用进程内 Go 正则，不 spawn `rg`
在 bwrap 沙箱里 spawn `rg` 会引入：进程外依赖（镜像要有 rg）、跨用户沙箱启动开销、输出格式解析成本，且 xizhi 工具按设计是**进程内、绑定用户 workspace** 的（无沙箱）。用标准库 `filepath.WalkDir` + `regexp`（RE2）+ `bufio.Scanner` 在进程内读匹配，行为可控、零依赖、与 `xizhi_glob_files` 的实现姿势一致。
- *备选*：spawn `rg`——被否，理由如上。
- *正则引擎*：用 Go `regexp`（RE2）。RE2 不支持反向引用但保证线性时间，对 agent 输入的任意 pattern 是安全选择（防 ReDoS）。字面量搜索是正则子集，无需单独模式。

### D2. grep 结果防爆：匹配数上限 + 行截断 + 跳二进制
agent 可能搜出成千上万匹配（如搜 `the`），直接全返会撑爆上下文。策略：硬上限 ~200 条匹配、每行截断 ~500 字符、超限置 `truncated: true`。二进制文件按 NUL 字节检测跳过（对齐 `grep -I` 与 workspace `WriteContent` 的二进制判定）。文件名过滤用可选 `glob`（doublestar，复用 `xizhi_glob_files` 同款），`ignore_case` 为独立布尔开关。上下文行用 `context_before`/`context_after` 双参数（默认各 0），比单参数对称式更灵活，覆盖 `-A`/`-B` 全部场景。

### D3. 移除 python/pip_install，bash 网络默认放开
`python`（`python3 -c`/`python3 file`）与 `pip_install`（`python3 -m pip install --target /workspace/.pip`）都是 `bash` 可直接表达的特化，专用工具的入参 schema（`code`/`file` 互斥、`packages` 数组）反而僵化。砍掉它们把执行器收敛为单一 `bash`。
- **PYTHONPATH 桥保留**：`bwrap.go` 已对每个沙箱无条件注入 `PYTHONPATH=/workspace/.pip`，故 bash 跑 `python3 -m pip install --target /workspace/.pip X` 后，下次 bash 跑 `python3` 仍能 `import X`。该不变量**不改实现**，只是从「跨工具」收敛为「bash 内」。
- **网络默认 `true`**：旧 `pip_install` 默认 `network: true`，砍掉它后要开箱支持 `pip install` via bash，须把 `tools.executor.bash.network` 默认由 `false` 改 `true`。**安全取舍已接受**：`bash + network` 比 `pip + network` 攻击面大（bash 可 curl 任意地址、外传工作区文件），本环境接受该放宽。真正的破坏性边界仍由 bwrap 命名空间 + Landlock（一切囚于 `/workspace`）兜底，`dangerousCommandPattern` warn audit 提供可观测性。
- *备选*：保留 `pip_install` 作为唯一窄网络出口——被否，用户明确选择「网络放开」并精简为单一 bash。
- 配置兼容：删除 `ExecutorConfig.Python`/`Pip` 字段后，老 `config.yaml` 残留 `python:`/`pip:` 块被非严格解析静默忽略。

### D4. bash 引导走纯提示词，**不做代码入参拦截**
用户原希望「在代码里校验禁用命令」。本设计明确否决代码拦截，理由：
- bash 是图灵完备语言，命令字符串无法可靠解析。任何子串/正则检测同时面临**误报**（文件名 `grep_error.log`、字符串字面量 `"don't use cat"`）与**漏报**（`/usr/bin/grep`、引号拼接 `gr""ep`、命令替换 `$(grep ...)`、`alias x=grep`、`bash -c "..."` 嵌套、转义 `g\rep`、换语言 `python3 -c "os.system('rm -rf x')"`）。硬拦只制造安全错觉，一戳即穿。
- **区分两类目标**：(a) **安全**（`rm`/`curl`/`wget`/`sudo`/`sshd`）——沙箱本身已兜底（一切囚于 `/workspace`），在命令串拦 `rm` 是安全戏法；现有 `dangerousCommandPattern` warn-only audit 足够观测。(b) **引导**（`cat`/`ls`/`find`/`grep`/`sed`/`awk`）——目标是让模型用更省 token 的专用工具，应靠**提示词 + 让专用工具足够好用**（本变更的 grep、read-any-file 正是为此），而非代码拦截。且 `sed`/`awk` 做批量文本变换是 `xizhi_modify_file`（单处精确替换）替代不了的，硬拦会伤能力。
- 落地：仅扩充 `bash` 工具描述的让位名单（`cat`/`rm`/`ls`/`find`/`sed`/`awk`/`grep`），逐个指向专用工具；`dangerousCommandPattern` 不动。

### D5. luban 放开扩展名，二进制按 NUL 字节拒绝
放开 `.md` 限制后，模型可能读到图片/数据等二进制。`ReadPath` 返回 `string`，二进制按 UTF-8 解会乱码并污染上下文。
- *方案 B（采用）*：检测 NUL 字节 → 拒绝，返回明确错误（对齐 workspace `WriteContent` 的 `BINARY_FILE` 判定）。文本资产全可读，二进制清晰报错。
- *备选 A（全放开返 string）*：被否，二进制返乱码。
- *备选 C（扩展名白名单）*：被否，僵化、新类型要改代码。
- frontmatter 剥离保持现状（`parseFrontmatter` 仅当文件以 `---` 开头才剥，对非 md 自动 no-op，安全）；500KB 上限与路径/符号链接安全校验不变。

## Risks / Trade-offs

- **[bash + network 放宽攻击面]** → 接受（用户决策）。缓解：bwrap 命名空间 + Landlock 把一切囚于 `/workspace`；`dangerousCommandPattern` warn audit 保留可观测性；operator 仍可经 `tools.executor.bash.network: false` 收紧。
- **[BREAKING：agent 引用 python/pip_install]** → 在 design/CLAUDE.md 显式标注迁移：把 agent 工具列表里的 `python`/`pip_install` 改为 `bash`，pip 用法变为 `python3 -m pip install --target /workspace/.pip <pkg>`。
- **[grep 大工作区性能]** → 进程内扫描大目录可能慢。缓解：按文件逐行 `bufio.Scanner`（不全量入内存）、跳二进制、结果上限早停；工作区规模本就受 per-user 数据约束，可接受。
- **[提示词引导非强制，模型仍可能用 bash cat]** → 接受。专用工具（尤其新 grep）足够省 token 时，模型自然倾向它们；硬拦的代价（误报/漏报/伤能力）高于收益。
- **[luban 放开后误读大二进制被 500KB 上限挡]** → 非风险，上限本就存在且保留；超限返回明确错误。

## Migration Plan

1. 代码改动（见 tasks）后，`make build && make test`。
2. **agent 配置迁移**：把 `config.yaml` 中任何 agent 的 `tools:` 里 `python`/`pip_install` 改为 `bash`。
3. **config.yaml 清理（可选）**：删除 `tools.executor.python` / `tools.executor.pip` 段（残留会被静默忽略）；如需收紧网络，显式设 `tools.executor.bash.network: false`。
4. **回滚**：纯代码变更，回滚 commit 即可恢复 python/pip 工具与 `.md` 限制；无 schema/数据迁移。
