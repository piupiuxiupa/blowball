## REMOVED Requirements

### Requirement: Python code execution tool
**Reason**: `python`（`python3 -c` / `python3 file`）是 `bash` 可直接表达的特化形态，专用工具的 `code`/`file` 互斥入参反而僵化。本变更把执行器收敛为单一 `bash`，Python 代码统一经 `bash` 调用（`python3 ...`）。
**Migration**: 把 agent 工具列表里的 `python` 改为 `bash`；内联代码用 `bash -c 'python3 -c "..."'` 或写入工作区文件后 `python3 <file>` 执行。`tools.executor.python` 配置块被非严格解析静默忽略，可删除。

### Requirement: Python package installation tool
**Reason**: `pip_install` 同样是 `bash` 的特化。保留对每个沙箱无条件注入的 `PYTHONPATH=/workspace/.pip` 后，`python3 -m pip install --target /workspace/.pip <pkg>` 经 bash 执行即可，且装好的包对后续 bash 内运行的 python3 直接可导入。
**Migration**: 把 agent 工具列表里的 `pip_install` 改为 `bash`；装包用 `bash` 执行 `python3 -m pip install --target /workspace/.pip <pkg>`。`tools.executor.pip` 配置块（含 `index_url`/`extra_index_urls`/`trusted_hosts`）被静默忽略——如需自定义 PyPI mirror/trusted host，改为在 agent 命令里显式传 `-i`/`--trusted-host`。

### Requirement: Installed packages visible to python tool
**Reason**: 该要求绑定已移除的 `python` 工具。`PYTHONPATH=/workspace/.pip` 注入对 `bash` 沙箱本就生效（见 `bwrap.go`），故 pip-via-bash 装的包对 bash 内 python3 仍可导入，无需独立要求。
**Migration**: 无需迁移——经 `bash` 运行 `python3 -m pip install --target /workspace/.pip X` 后，同一用户后续 `bash` 内 `python3 -c "import X"` 即可成功（`PYTHONPATH` 桥保留）。

## MODIFIED Requirements

### Requirement: Executor configuration
系统 SHALL 从 `config.yaml` 的 `tools.executor.bash` 读取 bash 执行器配置。`tools.executor.python` 与 `tools.executor.pip` 字段不再被消费（残留配置块被非严格解析静默忽略）。`tools.executor.bash.network` 默认值为 `true`（对齐旧 `pip_install` 的默认网络姿态，使 `pip install` via bash 开箱即用）。

#### Scenario: Enable bash tool
- **WHEN** `tools.executor.bash.enabled` 为 `true`
- **THEN** `bash` 工具被注册到工具注册表，对配置了该工具的 Agent 可见

#### Scenario: Configure timeout and output limit
- **WHEN** `tools.executor.bash.timeout` 设为 `30s` 且 `max_output_bytes` 为 `65536`
- **THEN** bash 命令在 30 秒后被终止
- **AND** 输出在 65536 字节处截断

#### Scenario: python/pip config blocks are ignored
- **WHEN** `config.yaml` 仍残留 `tools.executor.python` 或 `tools.executor.pip` 块
- **THEN** 系统启动正常，这些块被静默忽略，不注册任何 `python`/`pip_install` 工具

### Requirement: Network isolation
系统 SHALL 在 `network` 被显式禁用时让沙箱命令无网络访问。`tools.executor.bash.network` 默认为 `true`（bash 默认有网络）；operator 可经 `tools.executor.bash.network: false` 收紧。

#### Scenario: Network disabled
- **WHEN** `tools.executor.bash.network` 为 `false`
- **THEN** bwrap 命令包含 `--unshare-net`

#### Scenario: Network enabled
- **WHEN** `tools.executor.bash.network` 为 `true`
- **THEN** bwrap 命令不包含 `--unshare-net`

#### Scenario: Network enabled by default for bash
- **WHEN** `tools.executor.bash` 省略 `network` 字段
- **THEN** bwrap 命令不包含 `--unshare-net`（默认放开），使 `pip install` via bash 可达 PyPI

### Requirement: Sandbox /tmp mapped to workspace tmp directory
`bash` 沙箱 SHALL 把用户的 `workspace/tmp/` 目录挂载到沙箱内 `/tmp`，使临时文件在沙箱退出后持久化、并经 `xizhi_*` 工作空间工具可达。

#### Scenario: Bash writes temporary file to /tmp
- **WHEN** Agent 调用 `bash`，命令为 `{"command": "echo hello > /tmp/hello.txt"}`
- **THEN** 文件写入 `data/{user_uuid}/workspace/tmp/hello.txt`
- **AND** 随后 `xizhi_read_file` 以 path `tmp/hello.txt` 返回该内容

#### Scenario: workspace/tmp created on demand
- **WHEN** Agent 调用 `bash` 且 `workspace/tmp/` 尚不存在
- **THEN** 系统在挂载沙箱前创建 `workspace/tmp/`
- **AND** 命令执行成功

### Requirement: Home directory in sandbox
`bash` 沙箱 SHALL 在 bubblewrap 命名空间内提供一个真实、可写的 `$HOME` 目录，使在 `$HOME` 下缓存或配置的命令（如 pip `~/.cache`、`~/.config`）正常工作。沙箱 SHALL 强制把 `HOME` 环境变量设为该合成 home 路径，覆盖 `allowed_env_patterns`，压制任何会泄露进沙箱的宿主 `HOME`。

#### Scenario: Home directory is writable
- **WHEN** Agent 调用 `bash`，命令为 `{"command": "echo x > $HOME/.cache/foo && cat $HOME/.cache/foo"}`
- **THEN** 命令成功并打印 `x`
- **AND** `$HOME` 解析为沙箱内一个已挂载、可写的路径

#### Scenario: HOME is forced to the synthetic path
- **WHEN** Agent 调用 `bash`，命令为 `{"command": "echo $HOME"}`
- **THEN** 输出为合成 home 路径（如 `/home/blowball`），而非宿主用户主目录
- **AND** 即使 `allowed_env_patterns` 含 `HOME` 也如此

#### Scenario: Host HOME does not leak when filtered out
- **WHEN** `allowed_env_patterns` 不含 `HOME`
- **AND** Agent 调用 `bash`，命令为 `{"command": "echo $HOME"}`
- **THEN** 输出仍为合成 home 路径（HOME 被强制而非继承）

### Requirement: Operator tools directory on PATH
`bash` 沙箱 SHALL 把操作员工具目录 `{data-dir}/tools` 只读挂载到 `$HOME/.local/bin`（位于上述合成 home 内），并 SHALL 把 `$HOME/.local/bin` 前置到 `PATH`，使操作者的工具可裸名调用且优先于宿主 `/usr/bin`。建立 `$HOME` 的 `--tmpfs` SHALL 出现在工具目录 `--ro-bind` 之前，使挂载点存在。

#### Scenario: Operator tool invoked by bare name
- **WHEN** 可执行文件 `mytool` 存在于 `{data-dir}/tools`
- **AND** Agent 调用 `bash`，命令为 `{"command": "mytool --version"}`
- **THEN** 命令经 `PATH` 从 `$HOME/.local/bin` 解析到 `mytool` 并执行
- **AND** 返回合并的 stdout/stderr 与退出码

#### Scenario: Tools resolve via hardcoded $HOME/.local/bin lookup
- **WHEN** 沙箱内某工具直接查找 `$HOME/.local/bin/<binary>`（非经 `PATH`）
- **AND** 该 binary 存在于 `{data-dir}/tools`
- **THEN** 查找成功，因 `$HOME/.local/bin` 由只读 bind mount 填充

#### Scenario: Tools directory is read-only in the sandbox
- **WHEN** Agent 调用 `bash`，命令为 `{"command": "touch $HOME/.local/bin/evil"}`
- **THEN** 命令失败，因 `$HOME/.local/bin` 只读挂载

#### Scenario: PATH is prepended with tools bin
- **WHEN** Agent 调用 `bash`，命令为 `{"command": "echo $PATH"}`
- **THEN** 首个 `PATH` 条目为 `$HOME/.local/bin`
- **AND** 其余为经 `allowed_env_patterns` 过滤后的宿主 `PATH`（当 `PATH` 被允许时）

#### Scenario: Empty tools directory still sets up home and PATH
- **WHEN** `{data-dir}/tools` 存在但为空
- **AND** Agent 调用 `bash`，命令为 `{"command": "echo $PATH"}`
- **THEN** `$HOME/.local/bin` 仍存在且在 `PATH` 首位
- **AND** `$HOME` 仍为合成可写 home

### Requirement: Configurable executor sandbox mounts and system baseline
bwrap 沙箱 SHALL 读取 `tools.executor.sandbox` 配置（见 `sandbox-directory-configuration` 规格），在每次 executor 命令执行时追加绑定 operator 配置的额外挂载，并使用可配的系统只读基线。系统基线条目经 `os.Stat` 守卫，仅绑定实际存在的目录。承载性不变量 `/workspace`、`/home/blowball`、`$HOME/.local/bin`、`/skills/global`、`/skills/user`、`/tmp`、`/proc`、`/dev`、`PYTHONPATH`（`/workspace/.pip`）前缀逻辑、`--chdir /workspace` 的沙箱内目标路径保持固定，不受配置影响。未配置时挂载表与系统基线复现既有行为。

#### Scenario: 默认配置复现既有沙箱挂载
- **WHEN** 配置省略 `tools.executor.sandbox` 且 agent 调用 `bash`
- **THEN** 沙箱系统基线为只读绑定的 `["/usr","/bin","/lib","/lib64","/etc"]`，工作区挂到 `/workspace`，且不追加任何额外挂载
- **AND** 现有 bash 工具需求中的挂载场景对默认配置仍然成立

#### Scenario: 额外只读挂载在沙箱内可访问
- **WHEN** `tools.executor.sandbox.extra_read_only` 配置为 `["/opt/models"]` 且 agent 调用 `bash` 执行读取 `/opt/models/weights.bin` 的命令
- **THEN** 沙箱把 `/opt/models` 只读绑定（target 同 `/opt/models`），命令成功读取该文件

#### Scenario: 额外可写挂载在沙箱内可写
- **WHEN** `extra_read_write` 配置为 `["/srv/cache"]` 且 agent 调用 `bash` 写入 `/srv/cache/out.txt`
- **THEN** 沙箱把 `/srv/cache` 可写绑定，写入成功并落到宿主 `/srv/cache/out.txt`

#### Scenario: 缺失系统基线目录不致沙箱启动失败
- **WHEN** 运行环境缺少 `/lib64` 且 `system_read_only` 使用默认值
- **THEN** bwrap 跳过对 `/lib64` 的 `--ro-bind`，沙箱正常启动并执行命令

### Requirement: PYTHONPATH bridge retained for pip-via-bash
系统 SHALL 对每个 `bash` 沙箱无条件注入 `PYTHONPATH=/workspace/.pip`（或把既有 `PYTHONPATH` 前置 `/workspace/.pip`），使经 `bash` 运行 `python3 -m pip install --target /workspace/.pip <pkg>` 安装的包，对同一用户后续 `bash` 沙箱内运行的 `python3` 直接可导入，无需 `sys.path` 操作。

#### Scenario: pip-via-bash package importable in later bash run
- **WHEN** Agent 调用 `bash` 执行 `python3 -m pip install --target /workspace/.pip requests`
- **AND** 同一用户随后调用 `bash` 执行 `python3 -c "import requests; print(requests.__version__)"`
- **THEN** 后一次命令成功，输出已安装的 `requests` 版本

#### Scenario: PYTHONPATH set in bash sandbox
- **WHEN** Agent 调用 `bash`，命令为 `{"command": "echo $PYTHONPATH"}`
- **THEN** 输出含 `/workspace/.pip`（前置或独占）

### Requirement: Executor tool descriptions declare result shape, limits, and file-tool anti-pattern
`bash` 的工具描述 SHALL 声明其结果结构为 `{output, exit_code, truncated}`（`output` 为合并的 stdout+stderr），并告知输出有上限（默认 64KB，超限截断并以 `...output truncated...` 标记、置 `truncated: true`）与超时（默认 30s）。`bash` 描述 SHALL 包含一条扩充的反模式，以强指令词 `DO NOT` 标记：工作区文件的读取/列出/搜索/修改/删除**不要**直接用 `cat`、`rm`、`ls`、`find`、`sed`、`awk`、`grep`，而用对应的 `xizhi_*` 专用工具（`cat`→`xizhi_read_file`；`ls`→`xizhi_list_files`/`xizhi_tree`；`find`→`xizhi_glob_files`；`grep`→`xizhi_grep`；`sed`/`awk`→`xizhi_modify_file`；`rm`→`xizhi_delete`），除非专用工具无法完成该任务。该反模式为**纯提示词引导，系统不在代码层对命令入参做拦截**（bash 图灵完备，子串/正则检测必然误报与漏报；破坏性操作由沙箱与 warn audit 兜底）。描述 SHALL 对其致命约束（如 64KB 截断）用加粗 + 大写强指令词（`MUST`/`DO NOT`/`IMPORTANT` 等）标记不少于 2 处。

#### Scenario: bash 描述声明结果结构与上限
- **WHEN** `bash` 工具被注册并渲染给模型
- **THEN** 描述包含 `output`、`exit_code`、`truncated` 三个字段名，以及输出上限（64KB）与截断标记的说明

#### Scenario: bash 描述包含扩充的文件工具让位反模式
- **WHEN** `bash` 工具被注册并渲染给模型
- **THEN** 描述中以强指令词 `DO NOT` 标记让位反模式，名单覆盖 `cat`/`rm`/`ls`/`find`/`sed`/`awk`/`grep`，并逐个指向 `xizhi_read_file`/`xizhi_list_files`/`xizhi_tree`/`xizhi_glob_files`/`xizhi_grep`/`xizhi_modify_file`/`xizhi_delete`

#### Scenario: 反模式为纯提示词，无代码拦截
- **WHEN** Agent 调用 `bash` 执行含让位关键词的命令（如 `cat tmp/a.txt`）
- **THEN** 命令照常执行，系统不在代码层因关键词拦截或改写命令（引导仅来自工具描述）
