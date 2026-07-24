## 1. Config 结构与默认值（`internal/config/config.go`）

- [x] 1.1 新增 `LandlockConfig` 结构：`Enabled *bool`（默认 true，参照 `PipToolConfig.Network` 的 nil→默认模式）、`SystemReadOnly []string`、`ExtraReadWrite []string`、`ExtraReadOnly []string`；附 `IsEnabled()`（nil 时返回 true）
- [x] 1.2 新增 `ExecutorSandboxConfig` 结构：`SystemReadOnly []string`、`ExtraReadOnly []string`、`ExtraReadWrite []string`；以及解析后的 `ExtraReadOnlyMounts/ExtraReadWriteMounts []MountSpec`（`MountSpec{Host, Target string}`）
- [x] 1.3 新增 `DefaultLandlockSystemReadOnly()`（返回 `["/etc","/usr","/bin","/lib","/lib64","/proc"]`）与 `DefaultExecutorSystemReadOnly()`（返回 `["/usr","/bin","/lib","/lib64","/etc"]`）
- [x] 1.4 实现 `LandlockConfig.applyDefaults()`（空 `SystemReadOnly` 填默认；`Enabled` nil 不在此处理，由 `IsEnabled` 兜底）与 `ExecutorSandboxConfig.applyDefaults()`（空 `SystemReadOnly` 填默认）
- [x] 1.5 实现 `ParseMounts([]string) ([]MountSpec, error)`：支持 `host`（target=host）与 `host:target`；非绝对 host 报错
- [x] 1.6 在根 `Config` 增加 `Landlock LandlockConfig`；在 `ExecutorConfig` 增加 `Sandbox ExecutorSandboxConfig`
- [x] 1.7 在 `Load()` 中调用 `cfg.Landlock.applyDefaults()`、`cfg.Tools.Executor.Sandbox.applyDefaults()`，并在校验通过后用 `ParseMounts` 填充 `*Mounts` 字段（fail-fast：解析错误即启动失败）

## 2. Config 校验守卫（`internal/config/config.go`，对应 spec“配置校验守卫”）

- [x] 2.1 `landlock.enabled` 为真且有效 RW 集合（默认应用 RW 目录 ∪ `extra_read_write`）为空时返回错误
- [x] 2.2 所有 landlock/bwrap 配置目录与额外挂载 `host` 必须绝对路径，否则报错
- [x] 2.3 `landlock.extra_read_write` 与 `sandbox.extra_read_write` 包含 `"/"` 时报错
- [x] 2.4 额外挂载 `target` 与固定不变量（`/workspace`、`/home`、`/skills`、`/tmp`、`/proc`、`/dev`）或 `system_read_only` 条目冲突时报错

## 3. Landlock 参数化（`internal/tool/xizhi/`）

- [x] 3.1 `landlock.go`：`ApplyLandlock` 签名增加 `systemRODirs []string` 参数并透传
- [x] 3.2 `landlock_linux.go`：用 `systemRODirs`（经 stat 过滤）替换硬编码的 `RODirs("/etc","/usr","/bin","/lib","/lib64","/proc")`；缺失条目跳过并 `log.Warn`（保留 V2.BestEffort）
- [x] 3.3 `landlock_other.go`：`applyLandlock` 签名同步增加参数（no-op）
- [x] 3.4 新增/复用 stat 过滤辅助（仅对存在的目录施加 RODirs）

## 4. bwrap 参数化与额外挂载（`internal/tool/executor/`）

- [x] 4.1 `bwrap.go`：抽出 stat 过滤辅助，`buildBwrapArgs` 改为遍历 `system_read_only` 仅 `--ro-bind` 存在的目录（替换行 71-75 硬编码块）
- [x] 4.2 `buildBwrapArgs` 在不变量挂载之后追加 `extra_read_only`（`--ro-bind host target`）与 `extra_read_write`（`--bind host target`）
- [x] 4.3 `Tools` 持有解析后的 sandbox 挂载（`ExecutorSandboxConfig` 或其 `*Mounts`）；`NewTools` 增收该参数
- [x] 4.4 `runner.go` 的 `run()` 把 sandbox 挂载透传给 `buildBwrapArgs`
- [x] 4.5 确认 `/workspace`、`$HOME`、`$HOME/.local/bin`、skills 目标、`--chdir /workspace`、PYTHONPATH 逻辑均未受影响（不变量固定）

## 5. 启动接线（`cmd/blowball/serve.go`）

- [x] 5.1 `setupRuntime`：组装 landlock 列表 `rw=[dataDir,logDir,skillsDir]+ExtraReadWrite`、`ro=[toolsDir]+ExtraReadOnly`、`systemRO=cfg.Landlock.SystemReadOnly`；`cfg.Landlock.IsEnabled()` 为真时调用 `ApplyLandlock(rw,ro,systemRO)`，否则 `log.Warn` 跳过
- [x] 5.2 `setupRuntime` 启动日志增加生效的 RW/RO/systemRO/extra 挂载清单（便于审计）
- [x] 5.3 executor 接线点（`serve.go:413` `executor.NewTools`）传入 `cfg.Tools.Executor.Sandbox`
- [x] 5.4 共享模式下确认 `CheckSharedBackend`/`ProbeFUSEWorkspace` 仍以 `dataDir` 为锚点，额外目录不介入

## 6. 文档

- [x] 6.1 `config.example.yaml`：新增 `landlock:` 块（含 `enabled`/`system_read_only`/`extra_*` 注释）与 `tools.executor.sandbox:` 块（含 host/host:target 示例）
- [x] 6.2 `CLAUDE.md`：在 Security 段与 Important conventions（Config / Workspace storage）补充可配 landlock/sandbox 目录说明与默认零变更
- [x] 6.3 如 `api/openapi.yaml` 涉及配置契约，同步补充（若无则跳过）

## 7. 测试

- [x] 7.1 `internal/config/config_test.go`：默认值复现现状（landlock 系统基线含 `/proc`、executor 基线不含）、各校验守卫场景（空 RW、`"/"`、相对路径、target 冲突）、`ParseMounts` 的 host/host:target 解析
- [x] 7.2 `internal/tool/xizhi/landlock_rotation_test.go`：新增“缺失系统基线目录被跳过”场景；既有 rotation/tools-RO 测试通过新签名
- [x] 7.3 `internal/tool/executor/bwrap_test.go`：`buildBwrapArgs` 追加 extra RO/RW（含 host:target target）、stat 跳过缺失基线；既有 `TestBuildBwrapArgsEmitsHomeAndToolsBindRegardlessOfToolsDir` 仍通过
- [x] 7.4 `make test`（含 `go test ./internal/...` 与 `test/integration/...`）全绿；`make lint` 通过
