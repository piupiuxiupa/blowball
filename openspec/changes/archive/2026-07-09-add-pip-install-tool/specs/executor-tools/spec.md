## ADDED Requirements

### Requirement: Python package installation tool
The system SHALL register a tool named `pip_install` that installs Python packages into the user's workspace using pip inside the bubblewrap sandbox.

#### Scenario: Successful package installation
- **WHEN** the agent calls `pip_install` with `{"packages": ["requests"], "upgrade": false}`
- **THEN** the system executes `pip install --target /workspace/.pip requests` inside the bwrap sandbox
- **AND** the package is installed under `data/{userID}/workspace/.pip/`
- **AND** the tool returns the pip stdout/stderr combined with the exit code

#### Scenario: Install multiple packages
- **WHEN** the agent calls `pip_install` with `{"packages": ["numpy", "pandas>=2.0"], "upgrade": false}`
- **THEN** the system installs all listed packages into `/workspace/.pip`
- **AND** the tool returns the combined output

#### Scenario: Upgrade installed packages
- **WHEN** the agent calls `pip_install` with `{"packages": ["requests"], "upgrade": true}`
- **THEN** the system executes `pip install --target /workspace/.pip --upgrade requests`
- **AND** the package is upgraded in `/workspace/.pip`

#### Scenario: pip_install tool description guides model usage
- **WHEN** the system renders OpenAI tools for an agent configured with `pip_install`
- **THEN** the tool description instructs the agent to use it when Python code fails with `ModuleNotFoundError` or `ImportError`

#### Scenario: pip_install is not registered when disabled
- **WHEN** `tools.executor.pip.enabled` is `false`
- **THEN** the `pip_install` tool is not registered in the tool registry

#### Scenario: pip_install requires network
- **WHEN** the agent calls `pip_install` and `tools.executor.pip.network` is `true`
- **THEN** the bwrap command does not include `--unshare-net`
- **AND** pip can reach the configured index URL

#### Scenario: pip_install uses configured PyPI mirror
- **WHEN** `tools.executor.pip.index_url` is set to `https://pypi.tuna.tsinghua.edu.cn/simple`
- **THEN** the system passes `-i https://pypi.tuna.tsinghua.edu.cn/simple` to pip

#### Scenario: pip_install uses extra index URLs
- **WHEN** `tools.executor.pip.extra_index_urls` contains `https://extra.example.com/simple`
- **THEN** the system passes `--extra-index-url https://extra.example.com/simple` to pip

#### Scenario: pip_install uses trusted hosts
- **WHEN** `tools.executor.pip.trusted_hosts` contains `pypi.tuna.tsinghua.edu.cn`
- **THEN** the system passes `--trusted-host pypi.tuna.tsinghua.edu.cn` to pip

#### Scenario: pip_install audit logging
- **WHEN** the agent calls `pip_install`
- **THEN** the system logs the command string, tool name, user ID, exit code, output byte size, and duration

### Requirement: Installed packages visible to python tool
The system SHALL make packages installed via `pip_install` available to the `python` tool without requiring the agent to modify `sys.path`.

#### Scenario: Python code imports installed package
- **WHEN** the agent has previously called `pip_install` with `{"packages": ["requests"], "upgrade": false}`
- **AND** the agent calls `python` with `{"code": "import requests; print(requests.__version__)"}`
- **THEN** the code executes successfully
- **AND** the output contains the installed version of `requests`

#### Scenario: PYTHONPATH set in python sandbox
- **WHEN** the agent calls `python`
- **THEN** the bwrap sandbox has `PYTHONPATH=/workspace/.pip` (or the existing `PYTHONPATH` appended with `/workspace/.pip`)

## MODIFIED Requirements

### Requirement: Executor configuration
The system SHALL read executor configuration from `config.yaml` under `tools.executor.bash`, `tools.executor.python`, and `tools.executor.pip`.

#### Scenario: Enable pip_install tool
- **WHEN** `tools.executor.pip.enabled` is `true`
- **THEN** the `pip_install` tool is registered in the tool registry and visible to configured agents

#### Scenario: Configure pip timeout and output limit
- **WHEN** `tools.executor.pip.timeout` is set to `120s` and `max_output_bytes` is `65536`
- **THEN** pip commands are terminated after 120 seconds
- **AND** output is truncated at 65536 bytes

#### Scenario: Configure pip mirror
- **WHEN** `tools.executor.pip.index_url` is set to `https://pypi.tuna.tsinghua.edu.cn/simple`
- **THEN** `pip_install` uses that URL as the package index
