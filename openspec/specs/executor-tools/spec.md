# Executor Tools Capability

## Purpose

TBD — Provides sandboxed command execution tools (`bash`) for agents, scoped to the user's workspace with configurable isolation, audit logging, and dangerous-command detection.

## Requirements

### Requirement: Bash command execution tool
The system SHALL register a tool named `bash` that executes a shell command inside a bubblewrap sandbox scoped to the user's workspace and read-only skill directories.

#### Scenario: Successful bash command
- **WHEN** the agent calls `bash` with `{"command": "echo hello"}`
- **THEN** the system executes `bash -c 'echo hello'` inside a bwrap sandbox with working directory `/workspace` bound to the user's workspace
- **AND** the global skills directory is mounted read-only at `/skills/global`
- **AND** the per-user skills directory is mounted read-only at `/skills/user`
- **AND** the tool returns the command's stdout and stderr combined with the exit code

#### Scenario: Bash command timeout
- **WHEN** the agent calls `bash` with a command that runs longer than the configured timeout
- **THEN** the system terminates the sandbox process
- **AND** the tool returns an error indicating the command timed out

#### Scenario: Bash command output limit
- **WHEN** the agent calls `bash` and the combined output exceeds `max_output_bytes`
- **THEN** the system truncates the output to `max_output_bytes`
- **AND** the tool returns the truncated output with a marker indicating truncation

#### Scenario: Bash command outside workspace and skill directories access denied
- **WHEN** the sandboxed command attempts to read or write a path outside `/workspace`, `/skills/global`, or `/skills/user`
- **THEN** the access is denied by the bwrap filesystem isolation
- **AND** the tool returns the command's error output

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

### Requirement: Environment variable filtering
系统 SHALL 按三层优先级构造 `bash` 沙箱的环境变量（从低到高）：(1) **host allowlist 层** —— 仅放行 `os.Environ()` 中 key 匹配 `tools.executor.bash.allowed_env_patterns`（glob，`filepath.Match` 语法）的变量；(2) **operator 字面值层** —— 注入 `tools.executor.bash.env`（见「Operator-defined environment literals」）的 `KEY: value`，覆盖同名的 host allowlist 变量；(3) **强制不变量层** —— 系统始终最后应用且始终胜出：`HOME` 强制为合成沙箱 home（`/home/blowball`），`PATH` 前置 `$HOME/.local/bin`，`PYTHONPATH` 前置 `/workspace/.pip`。未配置 `env` 时，构造结果与既有行为一致。

#### Scenario: Secret variable not leaked
- **WHEN** 宿主进程设有 `OPENAI_API_KEY` 且其不在 `allowed_env_patterns` 中
- **AND** agent 调用 `bash` 执行 `env | grep OPENAI`
- **THEN** 命令输出不含 `OPENAI_API_KEY`

#### Scenario: Allowed variable passed
- **WHEN** `allowed_env_patterns` 含 `PATH` 且宿主环境设有 `PATH`
- **THEN** 沙箱命令可见 `PATH`（经 host allowlist 层放行）

#### Scenario: Operator env literal overrides allowed host variable
- **WHEN** `tools.executor.bash.env.FOO` 为 `"cfg-value"`
- **AND** 宿主环境 `FOO=host-value` 且 `FOO` 在 `allowed_env_patterns` 中
- **THEN** 沙箱命令看到的 `FOO` 为 `cfg-value`（字面值层覆盖 host allowlist 层）

#### Scenario: Forced PATH prepend wins over env literal
- **WHEN** `tools.executor.bash.env.PATH` 为 `/opt/bin`
- **AND** agent 调用 `bash` 执行 `echo $PATH`
- **THEN** 输出以 `$HOME/.local/bin` 开头，随后是 `/opt/bin`（强制层前置胜出）

#### Scenario: Forced PYTHONPATH prepend wins over env literal
- **WHEN** `tools.executor.bash.env.PYTHONPATH` 为 `/x`
- **AND** agent 调用 `bash` 执行 `echo $PYTHONPATH`
- **THEN** 输出以 `/workspace/.pip` 开头，随后是 `/x`（强制层前置胜出）

#### Scenario: No env block reproduces existing behavior
- **WHEN** `config.yaml` 省略 `tools.executor.bash.env`
- **THEN** `buildBwrapArgs` 产出的 `--setenv` 集合与本次变更前一致（零回归）

### Requirement: Operator-defined environment literals
系统 SHALL 读取 `tools.executor.bash.env`（一个 `KEY: value` map），把每一项作为字面值环境变量注入每个 `bash` 沙箱（覆盖同名 host allowlist 变量，被强制不变量层覆盖，见「Environment variable filtering」）。value 在 config load 阶段经全局 `${VAR}` / `${VAR:default}` 展开（与所有 config 字符串字段一致），故可写 `OPENAI_API_KEY: "${OPENAI_API_KEY}"` 引用宿主环境而不硬编码字面秘钥。系统 SHALL 在 config load 阶段 fail-fast 校验：(a) `HOME` 为保留键，出现在 `env` 中即报错（系统强制 `HOME` 为合成沙箱 home，operator 不可覆盖）；(b) 每个 key 名匹配 `^[A-Za-z_][A-Za-z0-9_]*$`，否则报错。`env` 为 operator 全局配置，注入到所有用户的 bash 沙箱；省略或为空时零行为变化。

#### Scenario: Literal env var injected into sandbox
- **WHEN** `tools.executor.bash.env.PIP_INDEX_URL` 为 `https://pypi.mirrors.example.com/simple`
- **AND** agent 调用 `bash` 执行 `echo $PIP_INDEX_URL`
- **THEN** 输出为 `https://pypi.mirrors.example.com/simple`

#### Scenario: Value expands host variable via ${VAR}
- **WHEN** `tools.executor.bash.env.HTTPS_PROXY` 为 `${CORP_PROXY}`
- **AND** 宿主环境 `CORP_PROXY=http://proxy.corp:3128`
- **THEN** 沙箱命令看到的 `HTTPS_PROXY` 为 `http://proxy.corp:3128`（config loader 全局展开）

#### Scenario: Value expands with default via ${VAR:default}
- **WHEN** `tools.executor.bash.env.LOG_LEVEL` 为 `${LOG_LEVEL:info}`
- **AND** 宿主环境未设 `LOG_LEVEL`
- **THEN** 沙箱命令看到的 `LOG_LEVEL` 为 `info`

#### Scenario: HOME reserved key rejected at config load
- **WHEN** `config.yaml` 的 `tools.executor.bash.env` 含 `HOME: /custom`
- **THEN** config load 失败并报错，说明 `HOME` 是保留键（被强制为合成沙箱 home）
- **AND** 系统不启动

#### Scenario: Invalid env key name rejected at config load
- **WHEN** `tools.executor.bash.env` 含 key `"bad key"`（含空格）、`""`（空）或 `"1ABC"`（数字开头）
- **THEN** config load 失败并报错，说明 key 名非法

#### Scenario: Empty env map is zero behavior change
- **WHEN** `tools.executor.bash.env` 省略或为空 map
- **THEN** 沙箱 env 构造与未配置 `env` 时完全一致

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

### Requirement: Audit logging
The system SHALL emit a structured audit log entry for every command execution.

#### Scenario: Log bash execution
- **WHEN** the agent calls `bash`
- **THEN** the system logs the command string, tool name, user ID, exit code, output byte size, and duration

### Requirement: Dangerous command detection
The system SHALL detect dangerous command patterns and emit a warning log entry.

#### Scenario: Dangerous command warning
- **WHEN** the agent calls `bash` with `{"command": "rm -rf /workspace/build"}`
- **THEN** the command executes
- **AND** the system logs a warning that the command contains a dangerous pattern

### Requirement: Linux-only availability
The system SHALL only register executor tools on Linux systems where `bwrap` is installed and unprivileged user namespaces are available.

#### Scenario: Missing bwrap
- **WHEN** the server starts on Linux and `bwrap` is not in `PATH`
- **THEN** executor tools are not registered
- **AND** the system logs a fatal error indicating `bwrap` is required

#### Scenario: Non-Linux platform
- **WHEN** the server starts on macOS or Windows
- **THEN** executor tools are not registered
- **AND** startup continues without error

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
