## Why

Landlock 的目录控制（进程级 RW/RO 目录与系统只读基线）与 bwrap 的目录映射（沙箱挂载表 + 系统只读基线）目前以字面量散落并硬编码在三处：`internal/tool/xizhi/landlock_linux.go`（系统基线 `/etc /usr /bin /lib /lib64 /proc`）、`internal/tool/executor/bwrap.go`（系统基线 `/usr /bin /lib /lib64 /etc` + 挂载映射）、`cmd/blowball/serve.go:setupRuntime`（landlock 的 RW/RO 应用目录选择）。operator 在不改代码的情况下无法：向沙箱暴露额外的只读数据集/模型缓存；在 NixOS 或库不在标准路径的发行版上调整系统基线；或为进程授予额外读写目录。

同时存在一个潜在缺陷：`internal/tool/executor/probe.go` 的 `systemROBindDirs` 已逐个 `os.Stat` 跳过缺失目录（兼容 aarch64 无 `/lib64`），而 `bwrap.go` 真正起沙箱时无条件 `--ro-bind /lib64`——在缺 `/lib64` 的环境上 bwrap 会因绑定不存在源路径而启动失败。将目录控制/映射配置化可同时满足弹性需求并统一两处基线、修复该不一致。

## What Changes

- 新增进程级 `landlock` 配置块：可配 `system_read_only`（系统只读基线，默认 `/etc`、`/usr`、`/bin`、`/lib`、`/lib64`、`/proc`）、`extra_read_write`、`extra_read_only`。进程 RW/RO 应用目录默认仍为 `{d}/data`、`{d}/logs`、`{d}/skills`（RW）与 `{d}/tools`（RO）。
- 新增 executor 级 `tools.executor.sandbox` 配置块：可配 `system_read_only`（默认 `/usr`、`/bin`、`/lib`、`/lib64`、`/etc`）、`extra_read_only`、`extra_read_write`（operator 额外的数据集/模型缓存挂载）。
- 系统基线统一改为“逐个 `os.Stat` 检查、仅绑定实际存在的目录”，与现有 probe 行为一致——修复 bwrap 在缺 `/lib64` 等环境上的启动失败隐患。
- 沙箱承载性不变量保持固定不可配：`/workspace`、`$HOME`（`/home/blowball`）、`$HOME/.local/bin`、skills 目标路径（`/skills/global`、`/skills/user`）不可更改，以免破坏 PYTHONPATH / `--chdir` / PATH 拼装逻辑。
- `validate()` 增加守卫：landlock 至少保留 1 个 RW 目录（保留现状检查）；`extra_read_write` 拒绝过宽路径（如 `/`）；共享模式下额外目录不得改变 `{data-dir}/data` 健康检查锚点。
- 所有默认值逐字段复现当前行为（零行为变更）；更新 `config.example.yaml` 与 `CLAUDE.md` 安全说明。

## Capabilities

### New Capabilities
- `sandbox-directory-configuration`: 定义 Landlock 与 bwrap 沙箱目录控制的配置契约——可配的系统只读基线（landlock/bwrap 共享概念）、landlock 的 RW/RO 目录、bwrap 的额外挂载，以及各自的默认值与校验守卫。

### Modified Capabilities
- `xizhi-tools`: “Landlock process-level restriction”需求改为目录来自配置（默认 = 当前字面量集合），系统只读基线可配、并按 stat 跳过缺失目录。
- `executor-tools`: bwrap 沙箱 SHALL 绑定 operator 配置的额外 RO/RW 挂载并使用可配的（stat 守卫的）系统基线；`/workspace`、`$HOME`、`$HOME/.local/bin`、skills 目标等承载性不变量保持固定。

## Impact

- 代码：`internal/config/config.go`（新增 `LandlockConfig`、`ExecutorSandboxConfig` 结构体、defaults 与 validate）；`internal/tool/xizhi/landlock_linux.go` 与 `landlock.go`（系统基线参数化）；`internal/tool/executor/bwrap.go`（系统基线参数化 + stat 守卫、追加额外挂载）；`cmd/blowball/serve.go` 的 `setupRuntime`（从配置读取并传参给 `ApplyLandlock`，executor 端把 sandbox 配置传入 `NewTools`/`buildBwrapArgs`）；`config.example.yaml`、`CLAUDE.md`。
- 安全：landlock 与 bwrap 属防御性安全层；配置化扩大了可控面，由默认值（零变更）与 `validate()` 守卫收敛。
- 共享存储：`workspace-shared-storage` 的 `{data-dir}/data` 健康检查与 bwrap FUSE 自检锚点保持不变；新增 `extra_*` 目录不影响该锚点。
- 向后兼容：无 BREAKING；未配置时行为与当前逐字节一致。`bwrap`/`landlock` 在 macOS/Windows 仍为 no-op。
