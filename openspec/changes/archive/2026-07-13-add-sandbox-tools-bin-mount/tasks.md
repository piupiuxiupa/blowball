## 1. Landlock read-only path class

- [x] 1.1 Refactor `applyLandlock` in `internal/tool/xizhi/landlock_linux.go` to take `(rwDirs, roDirs []string)` and call `landlock.RestrictPaths` with `RODirs("/etc","/usr","/bin","/lib","/lib64","/proc")`, `RODirs(roDirs...)`, and `RWDirs(rwDirs...)`; validate neither slice is empty where required.
- [x] 1.2 Update the non-Linux stub `internal/tool/xizhi/landlock_other.go` to the same `(rwDirs, roDirs)` signature (no-op warn).
- [x] 1.3 Change the exported `ApplyLandlock` entry point to accept read-write and read-only dir groups (e.g. `ApplyLandlock(rwDirs, roDirs []string) error`) and update its doc comment (D5/D6).
- [x] 1.4 Add/update `internal/tool/xizhi/landlock_rotation_test.go` to cover the RO path class, including a scenario asserting `{data-dir}/tools` is read-only while `data`/`logs`/`skills` remain read-write.

## 2. Runtime root wiring (serve.go)

- [x] 2.1 In `cmd/blowball/serve.go` derive `toolsDir := filepath.Join(dataRoot, "tools")` next to `dataDir`/`logDir`/`skillsDir`.
- [x] 2.2 `os.MkdirAll(toolsDir, 0o755)` after the skills-dir creation (api-server: "操作者工具目录始终被创建").
- [x] 2.3 Update the `xizhi.ApplyLandlock(...)` call to pass `rw=[]{dataDir, logDir, skillsDir}`, `ro=[]{toolsDir}`.
- [x] 2.4 Add `tools_dir` to the "runtime layout" zap log line.
- [x] 2.5 Pass `toolsDir` as a new argument to `executor.NewTools(...)`.

## 3. Executor tools-dir threading

- [x] 3.1 Add a `toolsDir string` field to `executor.Tools` in `internal/tool/executor/executor.go`.
- [x] 3.2 Add a `globalToolsDir string`/`toolsDir` parameter to `executor.NewTools` and store it.
- [x] 3.3 In `internal/tool/executor/runner.go`, pass `t.toolsDir` into the `buildBwrapArgs(...)` call.

## 4. Sandbox: home + tools bin + PATH

- [x] 4.1 Add a `toolsDir string` parameter to `buildBwrapArgs` in `internal/tool/executor/bwrap.go` and define the synthetic home constant (e.g. `sandboxHome = "/home/blowball"`).
- [x] 4.2 Emit `--tmpfs <sandboxHome>` BEFORE the tools `--ro-bind <toolsDir> <sandboxHome>/.local/bin` in the arg list (D2 ordering).
- [x] 4.3 After `filterEnv`, force `env["HOME"] = sandboxHome` (D3) and prepend `<sandboxHome>/.local/bin` to `env["PATH"]` (D4), preserving the existing PYTHONPATH logic.
- [x] 4.4 Confirm the build still produces exactly one `--setenv HOME ...` regardless of `allowed_env_patterns`.

## 5. Tests

- [x] 5.1 Extend `internal/tool/executor/bwrap_test.go`: assert `--tmpfs <home>` precedes `--ro-bind <toolsDir> <home>/.local/bin`.
- [x] 5.2 Assert `HOME` is forced to the synthetic path when `allowed_env_patterns` includes `HOME` and when it does not.
- [x] 5.3 Assert `$HOME/.local/bin` is the first `PATH` entry, with the inherited PATH appended when allowed.
- [x] 5.4 Add a test that an empty/non-empty `toolsDir` still emits the tmpfs home and the ro-bind (empty dir binds fine).
- [x] 5.5 Update/extend `test/integration/executor_test.go` if it asserts bwrap arg shape or sandbox env.
- [x] 5.6 Run `make test` (race detector) and `make lint`; fix any fallout.

## 6. Documentation

- [x] 6.1 Update `CLAUDE.md`: add `{data-dir}/tools` to the runtime layout, note the sandbox `$HOME` (tmpfs) + `$HOME/.local/bin` read-only bind + PATH prepend, and the landlock RW (data/logs/skills) vs RO (tools) split.
- [x] 6.2 Add a short note to `config.example.yaml` (executor section) describing the optional `{data-dir}/tools` directory and its effect.
- [x] 6.3 Document the `getpwuid()` limitation (deferred synthetic `/etc/passwd`) in the design or CLAUDE.md so it is discoverable.
