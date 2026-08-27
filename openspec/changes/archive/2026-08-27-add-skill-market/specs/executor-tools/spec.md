# Delta Spec: executor-tools

## ADDED Requirements

### Requirement: Skill market read-only mounts in bash sandbox

当 `skill_market.url` 启用（见 `skill-market` 能力）且 executor bash 工具可用时，bash 沙箱 SHALL 按**当前用户的 allowlist** 逐技能只读挂载市场目录：对 allowlist 中每个已授权**且磁盘上存在**的技能，追加一条 `--ro-bind {data-dir}/skills-market/{path} /skills/market/{name}`（目标路径按技能名拍平，不保留市场侧 user_id 层级；同名时取 allowlist 首个）。系统 SHALL NOT 将 `skills-market` 整目录挂载进沙箱——那会使任意用户经 bash 读到其他用户的市场技能。allowlist 为空或拉取失败（fail-closed）时 SHALL 挂载零个市场目录，bash 工具本身照常可用。磁盘上缺失的条目 SHALL 经 stat 守卫跳过（不使 bwrap 启动失败；该技能的脚本运行时才报 not-found）。挂载解析 SHALL 经 per-user TTL 缓存获取 allowlist，不因挂载逻辑产生额外出站请求。bash 工具描述 SHALL 说明市场技能的文件与脚本位于 `/skills/market/{skill-name}/`。

#### Scenario: 已授权技能的脚本可执行

- **WHEN** 用户的市场 allowlist 含 `fund-promo-sentiment-v2` 且磁盘已同步，agent 在 bash 中运行 `python3 /skills/market/fund-promo-sentiment-v2/scripts/run.py`
- **THEN** 沙箱内该路径可读、脚本可执行（只读），对应宿主 `{data-dir}/skills-market/{market_uid}/fund-promo-sentiment-v2/scripts/run.py`

#### Scenario: 其他用户的市场技能不可见

- **WHEN** `skills-market` 目录下存在其他 market user_id 的技能目录但不在当前用户 allowlist 中
- **THEN** 沙箱内不存在指向这些目录的任何挂载点，bash 无法访问其内容

#### Scenario: 磁盘滞后条目跳过挂载

- **WHEN** allowlist 含技能 `bar` 但宿主磁盘对应目录尚未同步
- **THEN** 该条目被跳过（无 `/skills/market/bar` 挂载点），bwrap 正常启动，bash 执行其他命令不受影响

#### Scenario: 市场不可达时零挂载

- **WHEN** 市场拉取失败（fail-closed 返回空 allowlist）
- **THEN** 沙箱内没有任何 `/skills/market/*` 挂载，bash 工具正常执行

#### Scenario: 工具描述声明市场路径约定

- **WHEN** bash 工具被注册并渲染给模型
- **THEN** 描述说明市场技能的文件与脚本位于 `/skills/market/{skill-name}/`（只读）
