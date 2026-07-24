## Context

Blowball 用两层防御性沙箱限制文件访问：

- **Landlock**（进程级，Linux）：`setupRuntime` 末尾对 server 进程本身施加 go-landlock V2 限制。当前 RW 目录为 `{d}/data`、`{d}/logs`、`{d}/skills`，RO 目录为 `{d}/tools`，外加硬编码的系统只读基线 `/etc /usr /bin /lib /lib64 /proc`（`internal/tool/xizhi/landlock_linux.go:42`）。
- **bwrap**（每命令级，Linux，executor 工具）：`buildBwrapArgs`（`internal/tool/executor/bwrap.go:60`）把工作区挂到 `/workspace`、skills 挂到 `/skills/{global,user}`、`$HOME` 合成到 `/home/blowball`、tools 挂到 `$HOME/.local/bin`，并硬编码系统只读绑定 `/usr /bin /lib /lib64 /etc`（行 71–75）。

两处基线字面量重复，且与 `probe.go` 的 `systemROBindDirs`（已是 `var` 且逐个 `os.Stat` 跳过缺失目录）行为不一致——真正起沙箱时 bwrap 无条件 `--ro-bind /lib64`，在缺 `/lib64` 的架构/发行版上会启动失败。同时承载性不变量（`/workspace`、`$HOME`、`$HOME/.local/bin`）被 PYTHONPATH（`/workspace/.pip`）、`--chdir /workspace`、PATH 前缀逻辑引用。

本设计把目录控制/映射配置化，目标是在不破坏不变量的前提下提供 operator 弹性，并统一两处基线、修复 stat 不一致。

## Goals / Non-Goals

**Goals:**
- operator 可不改代码地：向沙箱追加只读数据集/模型缓存（`extra_read_only`）、追加可写缓存（`extra_read_write`）、为进程追加额外 RW/RO 目录；调整 landlock 与 bwrap 各自的系统只读基线（适配 NixOS 等非标准布局）。
- 默认逐字段复现当前行为（零变更）。
- 统一 landlock 与 bwrap 的系统基线为“stat 守卫、仅绑定存在的目录”，修复 `/lib64` 缺失导致的 bwrap 启动失败。
- 用 `validate()` 守卫收敛配置化带来的可控面扩大（≥1 landlock RW 目录、拒绝过宽/非绝对路径、目标路径不与不变量/基线冲突）。
- 保持承载性不变量（`/workspace`、`$HOME`、`$HOME/.local/bin`、skills 目标）固定，避免 PYTHONPATH/`--chdir`/PATH 回归。

**Non-Goals:**
- 不把四个运行时目录（`data`/`logs`/`skills`/`tools`）从 `-d` 的固定子目录布局中解耦（自定义 log/skills 路径属更大改动，另行立项）。
- 不让承载性不变量（`/workspace`、`$HOME` 等）可配。
- 不改变 namespace flags（`--unshare-*`、`--proc`、`--dev`、`--chdir`）。
- 不触及 `workspace-shared-storage` 的健康检查/FUSE 自检逻辑（仅声明额外目录不影响其锚点）。
- 不提供运行时增删挂载的 API；配置仅在启动时生效。

## Decisions

### D1: 采用“分层 + 守卫”方案（C 档），而非纯加法或全声明式
- **选择**：系统只读基线可配（默认=现状，stat 守卫）+ 加法式额外挂载 + 承载性不变量保持固定。
- **替代 A（纯加法）**：基线不可改，无法满足 NixOS/非标准库布局需求，故排除。
- **替代 B（全声明式挂载表）**：operator 自行声明整张表，易漏 `/lib` 导致 exec 失败，且不变量变成 operator 负担、破坏 PYTHONPATH/PATH 假设，风险高，故排除。
- **理由**：基线与额外挂载覆盖了现实诉求（数据集、定制 distro），而保留不变量把高回归风险的耦合排除在可配面之外。

### D2: 两个独立配置块（landlock 进程级 / executor 沙箱级），而非统一块
- `landlock`（顶层）与 `tools.executor.sandbox`（executor 级）。
- **理由**：二者作用层不同（landlock 对 server 进程施加一次；bwrap 对每条命令施加），语义不同（landlock 的 `/proc` 只读 vs bwrap 的 `--proc` 合成）。统一块会错误暗示二者同作用域。
- 二者**共享“系统基线”概念**，但各持默认列表：landlock 含 `/proc`、`/etc`；bwrap 含 `/etc`，并另行合成 `/proc`、`/dev`。

### D3: 系统基线统一改为“stat 守卫、仅绑定存在的目录”
- landlock 与 bwrap 均对每个基线条目 `os.Stat`，缺失则跳过（与 `probe.go` 现行行为一致）。
- **理由**：修复 bwrap 在缺 `/lib64`（aarch64 等）环境上的启动失败；消除两份分叉实现。landlock 当前对缺失基线为 best-effort warn，stat 守卫属一致性加固。

### D4: 配置形态
```yaml
landlock:
  enabled: true                      # 默认 true；false 则完全跳过 ApplyLandlock（仅告警）
  system_read_only: [/etc, /usr, /bin, /lib, /lib64, /proc]   # 默认=现状；stat 守卫
  extra_read_write: []               # 额外进程可写目录（绝对路径）
  extra_read_only: []                # 额外进程只读目录（绝对路径）

tools:
  executor:
    sandbox:
      system_read_only: [/usr, /bin, /lib, /lib64, /etc]      # 默认=现状；stat 守卫
      extra_read_only: []            # operator 数据集，支持 "host" 或 "host:target"
      extra_read_write: []           # operator 可写缓存，支持 "host" 或 "host:target"
```
- 额外挂载条目支持两种字符串形式：`host`（target 同 host，与现有 `--ro-bind src src` 一致）或 `host:target`（自定义沙箱内路径，类 docker `-v`）。`target` 省略时 = `host`。
- landlock 的 `extra_*` 为 host 路径（无 target 概念）。

### D5: 默认值逐字段复现当前行为
- `landlock.system_read_only` 默认 `[/etc, /usr, /bin, /lib, /lib64, /proc]`（对齐 `landlock_linux.go:42`）。
- `tools.executor.sandbox.system_read_only` 默认 `[/usr, /bin, /lib, /lib64, /etc]`（对齐 `bwrap.go:71-75`）。
- `extra_*` 默认空。`applyDefaults()` 在零值时填入上述默认。

### D6: `validate()` 守卫
- `landlock.enabled` 为真且有效 RW 集合（`[data,logs,skills] ∪ extra_read_write`）为空 → 报错（保留 `applyLandlock` 现有“≥1 RW 目录”不变量）。
- 所有 landlock/bwrap 配置目录与额外挂载的 `host` 必须为绝对路径，否则报错。
- `extra_read_write` 拒绝 `"/"`（过宽）。
- 额外挂载的 `target` 不得与固定不变量（`/workspace`、`/home`、`/home/blowball`、`$HOME/.local/bin`、`/skills`、`/tmp`、`/proc`、`/dev`）或系统基线条目冲突，否则报错。

### D7: 接线
- `setupRuntime`：在派生四目录后组装 landlock 列表：`rw = [dataDir, logDir, skillsDir] + cfg.Landlock.ExtraReadWrite`；`ro = [toolsDir] + cfg.Landlock.ExtraReadOnly`；`systemRO = cfg.Landlock.SystemReadOnly`（stat 过滤）。调用 `ApplyLandlock(rw, ro, systemRO)`——签名新增 `systemRODirs` 参数。`cfg.Landlock.Enabled == false` 时跳过并告警。`landlock_other.go` 的 `applyLandlock` 签名同步增加该参数（no-op）。
- executor：`NewTools` 增收 sandbox 配置（或 `Tools` 持有解析后的 `SandboxMounts`）；`buildBwrapArgs` 改为从「stat 过滤的系统基线 + 固定不变量 + 额外挂载」组装参数。`extra_*` 的 host:target 解析在配置加载期完成（解析为结构体），运行时零解析。

### D8: 平台与向后兼容
- macOS/Windows：landlock no-op、executor 不注册；配置仍加载（默认值通过校验），不使用。
- 未配置任何新字段时行为与当前逐字节一致；无 BREAKING。

### D9: 共享存储交互（不变量）
- `workspace-shared-storage` 的 `CheckSharedBackend` 与 `executor.ProbeFUSEWorkspace` 始终以派生的 `dataDir` 为锚点，`extra_*` 目录不参与、不改变该锚点。作为新规格的一条 guardrail 需求落地。

## Risks / Trade-offs

- **[配置化削弱沙箱]** operator 误配（过宽 RW、删除基线）可能静默降低隔离强度。→ 默认零变更 + `validate()` 守卫（D6）+ 启动日志打印生效的 RW/RO/基线/额外挂载清单，便于审计。
- **[landlock stat 守卫改变既有失败语义]** 当前 landlock 对缺失 `/lib64` 为 best-effort warn 不致命；stat 跳过后语义更宽松（不再尝试限制不存在的路径）。→ 行为仅更宽松，不产生新的拒绝；保留 warn 日志。
- **[host:target 解析错误]** operator 写错分隔符或 target。→ 加载期解析失败即启动报错（fail-fast），运行时不接触原始字符串。
- **[target 冲突]** 额外挂载覆盖 `/workspace` 等不变量会破坏沙箱语义。→ D6 校验拒绝冲突 target。
- **[bwrap `--ro-bind` 缺失源仍可能致命]** stat 守卫降低概率，但 TOCTOU（stat 后挂载消失）理论存在。→ best-effort；与 probe 一致；不引入额外复杂度。

## Migration Plan

- 纯增量、默认零变更：升级后无需改 `config.yaml`；未填新字段时 `applyDefaults()` 填入当前字面量，行为不变。
- 回滚：删除 `landlock` 与 `tools.executor.sandbox` 块即恢复旧行为；代码层新参数有默认值，旧二进制与新配置互不依赖。
- 修复 `/lib64` 缺失环境：升级后 bwrap 在该等环境不再无条件绑定，自动可用。

## Open Questions

- `extra_read_only` 是否需要支持只读**可写混合**（如某挂载需 RW）？当前已分 `extra_read_only`/`extra_read_write` 两列，足以覆盖，暂不引入更细的 per-mount mode。
- 是否暴露 `landlock.enabled: false`？倾向“是”（比当前的 best-effort warn 更显式），但需在文档强调安全后果。
- 未来若要解耦四个运行时目录（自定义 log/skills 路径），应作为独立 change 推进，本设计预留 `extra_*` 不与之冲突。
