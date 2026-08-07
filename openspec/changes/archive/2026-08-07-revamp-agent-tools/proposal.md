## Why

当前 agent 工具层有三个可观察的痛点：(1) 工作区**只能按文件名找文件**（`xizhi_glob_files`），无法按内容搜索，模型定位「某个符号定义在哪」「哪些文件 import 了 X」只能逐个 `xizhi_read_file` 肉眼扫，极度费 token 且慢；(2) `luban_read_skill` 只能读 `.md`，而 skill 已是「仓库即包」的形态（`luban_install_skill` 会 `git clone` 整仓），其中的脚本/模板/示例/配置类文本资产读不出来；(3) `python` / `pip_install` 两个专用执行器与 `bash` 高度重叠，且 `bash` 描述里引导模型让位专用工具的名单不全。本次集中梳理工具层：新增内容搜索工具、放开 skill 资产读取、精简执行器、并用提示词（非代码）强化「专用工具优先」的引导。

## What Changes

- **新增 `xizhi_grep` 工作区内容搜索工具**（ripgrep 体验）：基于 RE2 正则搜索工作区内文件内容，支持文件名 glob 过滤、大小写忽略开关、匹配行行号、`context_before`/`context_after` 上下文行；跳过二进制文件、拒 `.blowball` 保留命名空间、不跟符号链接；结果设上限（默认 ~200 条匹配、每行截断 ~500 字符，超限置 `truncated: true`）。受 `tools.xizhi.grep.enabled` 开关控制，注册模式对齐 `xizhi_glob_files`。
- **`luban_read_skill` 放开扩展名限制**：从「仅 `.md`」改为「可读取 skill 目录树内任意文本文件」；检测到二进制文件（NUL 字节）时拒绝并返回明确错误（对齐 workspace `WriteContent` 的 `BINARY_FILE` 策略）；frontmatter 剥离保持现状（仅当文件以 `---` 开头才剥，对非 md 自动 no-op）；500KB 大小上限与路径/符号链接安全校验不变。**BREAKING**：此前被 `.md` 限制拒绝的非 md 文本路径，现在会成功读取。
- **移除 `python` 与 `pip_install` 专用执行器工具**：仅保留 `bash`。**BREAKING**——配置/工具列表中引用 `python`/`pip_install` 的 agent 将无法再注册这两个工具。Python 代码与 pip 安装统一经 `bash` 调用（`python3 ...` / `python3 -m pip install --target /workspace/.pip ...`）；保留既有对每个沙箱无条件注入的 `PYTHONPATH=/workspace/.pip`，使 pip-via-bash 安装的包对后续 bash 内运行的 python3 直接可导入。**`bash` 网络默认放开**：`tools.executor.bash.network` 默认值由 `false` 改为 `true`，使 `pip install` via bash 开箱即用（对齐旧 `pip_install` 的默认网络姿态）。配置中的残留 `tools.executor.python` / `tools.executor.pip` 块因非严格 YAML 解析被静默忽略，不致启动失败。
- **`bash` 工具描述强化「专用工具优先」引导（纯提示词，零代码拦截）**：把让位名单扩充为 `cat`、`rm`、`ls`、`find`、`sed`、`awk`、`grep`，并逐个指向专用工具（`xizhi_read_file` / `xizhi_list_files`+`xizhi_tree` / `xizhi_glob_files`+`xizhi_grep` / `xizhi_modify_file` / `xizhi_delete`）。**明确不在代码里做命令入参拦截**——bash 是图灵完备语言，正则/子串检测必然同时误报（文件名含关键词、字符串字面量）与漏报（绝对路径、引号拼接、命令替换、alias、`bash -c` 嵌套、换语言执行），硬拦只制造安全错觉。现有 `dangerousCommandPattern`（`rm`/`curl`/`wget`/`sudo`/`sshd`）的 warn-only audit 保持不动作为可观测性。

## Capabilities

### New Capabilities
- `xizhi-grep-files`: 工作区文件内容搜索工具 `xizhi_grep`（正则 + 文件名 glob 过滤 + 行号 + 上下文行 + 大小写开关），注册模式与安全边界对齐既有 `xizhi-glob-files`。

### Modified Capabilities
- `luban-skill-tools`: `luban_read_skill` 由「仅 `.md`」改为「任意文本文件，二进制拒绝」；同步更新 `path` 子文档读取与工具描述要求。
- `executor-tools`: 移除 `python` 执行器、`pip_install` 安装器及「已装包对 python 工具可见」要求；`bash.network` 默认改为 `true`；`bash` 工具描述反模式名单扩充并指向专用工具；保留 `PYTHONPATH=/workspace/.pip` 注入以支持 pip-via-bash；执行器配置/沙箱/tmp/$HOME/PATH 等要求改为仅描述 `bash`。
- `skill-directory-sandbox-access`: 移除对 `python` 工具的引用，沙箱内技能目录只读访问仅由 `bash` 承载。
- `system-prompt-rendering`: Skills 段落引导模型用 `bash`（而非 `bash` 或 `python`）访问技能目录文件。
- `api-server`: 操作员工具目录（`{data-dir}/tools`）用途描述由「为 `bash`/`python`/`pip_install` 提供 CLI」改为「为 `bash` 提供 CLI」。
- `workspace-shared-storage`: 跨节点 `.pip` 共享场景由「`pip_install` 装 + `python` 导入」改写为「经 `bash` 装 + 经 `bash` 内 python3 导入」（`PYTHONPATH=/workspace/.pip` 不变量不变）。

## Impact

- **代码**：`internal/tool/xizhi/` 新增 `grep.go` + `grep_test.go`，`register.go` 加 `NameGrep`/`schemaGrep`/注册分支，`XizhiConfig` 加 `Grep XizhiToolConfig`；`internal/tool/luban/read.go` + `register.go` 去除 `.md` 限制并加二进制拒绝；`internal/tool/skill/skill.go` 的 `validateSkillSubPath` 去扩展名校验、`ReadPath` 加二进制检测；`internal/tool/executor/` 删除 `registerPython`/`registerPip`/`schemaPython`/`schemaPip`/`buildPipCommand`/`resolvePythonFile`/`pythonArgs`/`pipArgs`/`ToolPython`/`ToolPip`，`executor.go` 的 `RegisterAll` 仅剩 bash 分支，`runner.go`/`bwrap.go` 的 PYTHONPATH 注入保留；`internal/config/config.go` 删除 `PipToolConfig` 类型与 `ExecutorConfig.Python`/`Pip` 字段（及 `Default*`/`ApplyDefaults`/`ToExecutorToolConfig` 等关联方法），`DefaultExecutorToolConfig().Network` 改 `true`，`executorConfigured()`（`cmd/blowball/serve.go`）仅判 `Bash.Enabled`。
- **配置 / 部署**：`config.example.yaml` 删除 `executor.python` / `executor.pip` 段，`executor.bash.network` 注释改为默认 true；`tools.xizhi.grep.enabled` 加示例（默认 true）。既有 `config.yaml` 残留 `python:`/`pip:` 块被静默忽略，operator 升级后可清理。
- **agent 配置**：引用 `python` / `pip_install` 的 agent 需改为 `bash`（BREAKING）。
- **文档**：`CLAUDE.md`（执行器段落、工具族列表、xizhi 工具列表、`needsLubanTools` 描述）需同步。
- **测试**：`internal/tool/executor/` 下涉及 python/pip 的测试（`executor_test.go`/`register_test.go`/`bwrap_test.go`）需删除/改写；新增 `xizhi_grep` 与 luban 非 md/二进制拒绝的测试。
- **依赖**：无新增第三方库（grep 用标准库 `regexp` + `filepath` + `bufio`；xizhi 已依赖 `doublestar`）。
