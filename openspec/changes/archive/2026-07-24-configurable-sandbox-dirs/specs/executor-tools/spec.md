## ADDED Requirements

### Requirement: Configurable executor sandbox mounts and system baseline
bwrap 沙箱 SHALL 读取 `tools.executor.sandbox` 配置（见 `sandbox-directory-configuration` 规格），在每次 executor 命令执行时追加绑定 operator 配置的额外挂载，并使用可配的系统只读基线。系统基线条目经 `os.Stat` 守卫，仅绑定实际存在的目录。承载性不变量 `/workspace`、`/home/blowball`、`$HOME/.local/bin`、`/skills/global`、`/skills/user`、`/tmp`、`/proc`、`/dev` 的沙箱内目标路径保持固定，不受配置影响。未配置时挂载表与系统基线复现既有行为。

#### Scenario: 默认配置复现既有沙箱挂载
- **WHEN** 配置省略 `tools.executor.sandbox` 且 agent 调用 `bash`/`python`
- **THEN** 沙箱系统基线为只读绑定的 `["/usr","/bin","/lib","/lib64","/etc"]`，工作区挂到 `/workspace`，且不追加任何额外挂载
- **AND** 现有 bash/python 工具需求中的挂载场景对默认配置仍然成立

#### Scenario: 额外只读挂载在沙箱内可访问
- **WHEN** `tools.executor.sandbox.extra_read_only` 配置为 `["/opt/models"]` 且 agent 调用 `bash` 执行读取 `/opt/models/weights.bin` 的命令
- **THEN** 沙箱把 `/opt/models` 只读绑定（target 同 `/opt/models`），命令成功读取该文件

#### Scenario: 额外可写挂载在沙箱内可写
- **WHEN** `extra_read_write` 配置为 `["/srv/cache"]` 且 agent 调用 `python` 写入 `/srv/cache/out.txt`
- **THEN** 沙箱把 `/srv/cache` 可写绑定，写入成功并落到宿主 `/srv/cache/out.txt`

#### Scenario: 缺失系统基线目录不致沙箱启动失败
- **WHEN** 运行环境缺少 `/lib64` 且 `system_read_only` 使用默认值
- **THEN** bwrap 跳过对 `/lib64` 的 `--ro-bind`，沙箱正常启动并执行命令
