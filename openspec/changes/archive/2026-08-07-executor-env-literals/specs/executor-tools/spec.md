## MODIFIED Requirements

### Requirement: Environment variable filtering
系统 SHALL 按三层优先级构造 `bash` 沙箱的环境变量(从低到高):(1) **host allowlist 层** —— 仅放行 `os.Environ()` 中 key 匹配 `tools.executor.bash.allowed_env_patterns`(glob,`filepath.Match` 语法)的变量;(2) **operator 字面值层** —— 注入 `tools.executor.bash.env`(见「Operator-defined environment literals」)的 `KEY: value`,覆盖同名的 host allowlist 变量;(3) **强制不变量层** —— 系统始终最后应用且始终胜出:`HOME` 强制为合成沙箱 home(`/home/blowball`),`PATH` 前置 `$HOME/.local/bin`,`PYTHONPATH` 前置 `/workspace/.pip`。未配置 `env` 时,构造结果与既有行为一致。

#### Scenario: Secret variable not leaked
- **WHEN** 宿主进程设有 `OPENAI_API_KEY` 且其不在 `allowed_env_patterns` 中
- **AND** agent 调用 `bash` 执行 `env | grep OPENAI`
- **THEN** 命令输出不含 `OPENAI_API_KEY`

#### Scenario: Allowed variable passed
- **WHEN** `allowed_env_patterns` 含 `PATH` 且宿主环境设有 `PATH`
- **THEN** 沙箱命令可见 `PATH`(经 host allowlist 层放行)

#### Scenario: Operator env literal overrides allowed host variable
- **WHEN** `tools.executor.bash.env.FOO` 为 `"cfg-value"`
- **AND** 宿主环境 `FOO=host-value` 且 `FOO` 在 `allowed_env_patterns` 中
- **THEN** 沙箱命令看到的 `FOO` 为 `cfg-value`(字面值层覆盖 host allowlist 层)

#### Scenario: Forced PATH prepend wins over env literal
- **WHEN** `tools.executor.bash.env.PATH` 为 `/opt/bin`
- **AND** agent 调用 `bash` 执行 `echo $PATH`
- **THEN** 输出以 `$HOME/.local/bin` 开头,随后是 `/opt/bin`(强制层前置胜出)

#### Scenario: Forced PYTHONPATH prepend wins over env literal
- **WHEN** `tools.executor.bash.env.PYTHONPATH` 为 `/x`
- **AND** agent 调用 `bash` 执行 `echo $PYTHONPATH`
- **THEN** 输出以 `/workspace/.pip` 开头,随后是 `/x`(强制层前置胜出)

#### Scenario: No env block reproduces existing behavior
- **WHEN** `config.yaml` 省略 `tools.executor.bash.env`
- **THEN** `buildBwrapArgs` 产出的 `--setenv` 集合与本次变更前一致(零回归)

## ADDED Requirements

### Requirement: Operator-defined environment literals
系统 SHALL 读取 `tools.executor.bash.env`(一个 `KEY: value` map),把每一项作为字面值环境变量注入每个 `bash` 沙箱(覆盖同名 host allowlist 变量,被强制不变量层覆盖,见「Environment variable filtering」)。value 在 config load 阶段经全局 `${VAR}` / `${VAR:default}` 展开(与所有 config 字符串字段一致),故可写 `OPENAI_API_KEY: "${OPENAI_API_KEY}"` 引用宿主环境而不硬编码字面秘钥。系统 SHALL 在 config load 阶段 fail-fast 校验:(a) `HOME` 为保留键,出现在 `env` 中即报错(系统强制 `HOME` 为合成沙箱 home,operator 不可覆盖);(b) 每个 key 名匹配 `^[A-Za-z_][A-Za-z0-9_]*$`,否则报错。`env` 为 operator 全局配置,注入到所有用户的 bash 沙箱;省略或为空时零行为变化。

#### Scenario: Literal env var injected into sandbox
- **WHEN** `tools.executor.bash.env.PIP_INDEX_URL` 为 `https://pypi.mirrors.example.com/simple`
- **AND** agent 调用 `bash` 执行 `echo $PIP_INDEX_URL`
- **THEN** 输出为 `https://pypi.mirrors.example.com/simple`

#### Scenario: Value expands host variable via ${VAR}
- **WHEN** `tools.executor.bash.env.HTTPS_PROXY` 为 `${CORP_PROXY}`
- **AND** 宿主环境 `CORP_PROXY=http://proxy.corp:3128`
- **THEN** 沙箱命令看到的 `HTTPS_PROXY` 为 `http://proxy.corp:3128`(config loader 全局展开)

#### Scenario: Value expands with default via ${VAR:default}
- **WHEN** `tools.executor.bash.env.LOG_LEVEL` 为 `${LOG_LEVEL:info}`
- **AND** 宿主环境未设 `LOG_LEVEL`
- **THEN** 沙箱命令看到的 `LOG_LEVEL` 为 `info`

#### Scenario: HOME reserved key rejected at config load
- **WHEN** `config.yaml` 的 `tools.executor.bash.env` 含 `HOME: /custom`
- **THEN** config load 失败并报错,说明 `HOME` 是保留键(被强制为合成沙箱 home)
- **AND** 系统不启动

#### Scenario: Invalid env key name rejected at config load
- **WHEN** `tools.executor.bash.env` 含 key `"bad key"`(含空格)、`""`(空)或 `"1ABC"`(数字开头)
- **THEN** config load 失败并报错,说明 key 名非法

#### Scenario: Empty env map is zero behavior change
- **WHEN** `tools.executor.bash.env` 省略或为空 map
- **THEN** 沙箱 env 构造与未配置 `env` 时完全一致
