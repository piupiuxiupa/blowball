## Context

The `bash`/`python`/`pip_install` executor tools run each command inside a fresh bubblewrap (`bwrap`) sandbox. The sandbox today mounts the host's `/usr`, `/bin`, `/lib`, `/lib64`, `/etc` read-only, the user's workspace read-write at `/workspace`, a workspace `tmp/` at `/tmp`, and the global/per-user skill directories read-only at `/skills/global` and `/skills/user`. The environment is cleared (`--clearenv`) and only host vars matching `allowed_env_patterns` are re-injected.

Operators want to supply a set of CLI binaries (e.g. `node`, data-fetch CLIs) that agents can call from inside the sandbox. Several of these tools **hardcode** `$HOME/.local/bin` as their lookup path, so for them to work the sandbox must:

1. resolve `$HOME` to a real, mounted path,
2. populate `$HOME/.local/bin` with the operator's tools (read-only), and
3. put `$HOME/.local/bin` on `PATH`.

The blocker: **the sandbox currently has no mounted home directory.** `$HOME` is re-injected from the host env only when `HOME` is in `allowed_env_patterns` (it is by default), but the host home path it points at is never mounted — so `$HOME` resolves to a path that does not exist inside the namespace, and any tool looking under it finds nothing.

At startup the server derives three runtime subdirs from the `-d`/`--data-dir` root — `data`, `logs`, `skills` — creates them, and applies a go-landlock V2 restriction covering all three as **read-write** (`applyLandlock(dirs []string)` → `RWDirs(...)`). There is no read-only path class today.

Reference code: `cmd/blowball/serve.go` (bootstrap), `internal/tool/executor/{bwrap,executor,runner,env}.go` (sandbox), `internal/tool/xizhi/landlock_{linux,other}.go` (landlock).

## Goals / Non-Goals

**Goals:**
- Provide an operator-managed tools directory (`{data-dir}/tools`) whose contents are available inside the sandbox at `$HOME/.local/bin`, read-only.
- Make `$HOME` a real, writable path inside the sandbox so commands that cache/config under `$HOME` (pip cache, shell history, `~/.config`) keep working.
- Put `$HOME/.local/bin` on `PATH` so tools are invocable by bare name.
- Cover `{data-dir}/tools` with read-only Landlock as defense-in-depth alongside the bwrap `--ro-bind`.
- Preserve all existing sandbox behavior (workspace, skills, /tmp, PYTHONPATH, env filtering, network isolation).

**Non-Goals:**
- No new config fields; `{data-dir}/tools` is a fixed subdir (mirrors `data`/`logs`/`skills`).
- No synthetic `/etc/passwd`. Tools that resolve home via `getpwuid()` instead of `$HOME` are handled only if a real tool proves to need it (see Open Questions).
- No per-user tools directory; the tools bin is process-global (operator-provided), shared across users, like the global skills dir.
- No frontend/API surface for uploading/managing tools; operators place files directly on disk.

## Decisions

### D1 — In-sandbox mount target is `$HOME/.local/bin`
**Choice:** bind `{data-dir}/tools` read-only onto `$HOME/.local/bin`.
**Why:** the motivating tools hardcode this path; any other target would not satisfy them. This is a user constraint, not a stylistic choice.
**Alternatives considered:**
- `/tools` (mirrors `/skills/global`): cleaner and consistent with existing mounts, but does not satisfy tools that look at `~/.local/bin`. Rejected on this basis (explored and discarded in `/opsx:explore`).
- `/usr/local/bin`: already on the inherited host `PATH`, but `--ro-bind`-ing over it shadows the host's real `/usr/local/bin` and is surprising. Rejected.

### D2 — `$HOME` is a writable ephemeral `--tmpfs`, not a host bind, not read-only
**Choice:** `--tmpfs <HOME>` establishes a writable in-namespace home, then `--ro-bind {tools} <HOME>/.local/bin` overlays the tools.
**Why:**
- *Writable* (not RO): other commands the agent runs may write to `$HOME` (pip `~/.cache`, tools reading `~/.config`). Today those writes silently fail (the path is absent); making `$HOME` writable is strictly more functional and avoids new failure modes.
- *Ephemeral tmpfs* (not a host bind): no host-side tempdir to create/clean per execution, no cross-execution state leakage, no workspace pollution. bwrap `--tmpfs` is in-namespace memory with zero host management.
- *Synthetic fixed path* (`/home/blowball`): deterministic; does not collide with any host home; does not require the host home to exist.
**Alternatives considered:**
- Bind a per-execution host tempdir as `$HOME`: works but adds create/cleanup in `run()` and host disk I/O for ephemeral caches. Rejected in favor of tmpfs.
- Bind the *host* home read-only and overlay `.local/bin`: invasive (shadows operator's real `~/.local/bin`), couples sandbox to host layout, leaks host home contents. Rejected.
**Bind ordering:** `--tmpfs <HOME>` MUST appear before `--ro-bind {tools} <HOME>/.local/bin` so the mountpoint exists when the ro-bind is applied. This is standard bwrap usage.

### D3 — `HOME` is forced in the sandbox env, overriding any host leak
**Choice:** after `filterEnv(allowed_env_patterns)` builds the env map, set `env["HOME"] = "/home/blowball"` unconditionally, before the `--setenv` loop.
**Why:** if `HOME` is in `allowed_env_patterns` (the default), the host home would otherwise leak into the sandbox and point at an unmounted path. Forcing it guarantees `$HOME` matches the tmpfs mount. Overwriting the map key yields exactly one `--setenv HOME ...` regardless of map iteration order. This holds whether or not the operator keeps `HOME` in the allow-list.
**Synthetic path chosen:** `/home/blowball` — project-aligned, obviously non-host. Not configurable in this change.

### D4 — `$HOME/.local/bin` is **prepended** to `PATH`
**Choice:** `PATH = $HOME/.local/bin : <existing PATH>` (or just `$HOME/.local/bin` when `PATH` is absent/empty).
**Why:** the operator deliberately provides these tools; they should take precedence over host `/usr/bin` shadows. Prepend matches operator intent. If `PATH` is not in `allowed_env_patterns`, the sandbox still gets `$HOME/.local/bin` as `PATH` (the operator's tools remain reachable); we do not inject the host `PATH` if the operator chose to filter it out.

### D5 — Landlock gains a read-only path class; `{data-dir}/tools` is RO
**Choice:** split the internal landlock helper into `(rwDirs, roDirs []string)` and call `RestrictPaths(RODirs(system...), RODirs(toolsDir), RWDirs(data, logs, skills))`. Expose via an updated `ApplyLandlock` (signature change) or a companion entry point.
**Why:** the tools dir is operator-provided static content; read-only is the correct, tighter restriction and matches the user requirement. Today `applyLandlock` only supports `RWDirs`, so an RO concept must be introduced. The existing `RODirs("/etc","/usr",...)` call already proves go-landlock composes RO+RW in one `RestrictPaths`.
**Alternatives considered:**
- Add `tools` to the existing `RWDirs` set (like skills today): simpler but looser than necessary; skills are RW under landlock only as an over-permissive accident (bwrap already mounts them RO). For a dedicated tools feature, RO is the deliberate choice. Rejected.
**Caller impact:** `ApplyLandlock` is called only from `cmd/blowball/serve.go` and the rotation test. The signature change is contained.

### D6 — `{data-dir}/tools` is a fixed subdir, always created and always mounted
**Choice:** derive `toolsDir := filepath.Join(dataRoot, "tools")`, `os.MkdirAll` at startup (next to `skills`), always pass to landlock (RO) and to `executor.NewTools`, always emit the tmpfs+ro-bind in `buildBwrapArgs`.
**Why:** mirrors the established `data`/`logs`/`skills` pattern and the `globalSkillsDir` threading through `NewTools` → `buildBwrapArgs`. An empty tools dir binds harmlessly (empty `$HOME/.local/bin`, an extra `PATH` entry). Uniform setup avoids conditional logic in the hot path. Always creating it keeps the "N runtime subdirs" mental model and lets landlock always cover it.
**Alternatives considered:**
- Only create/mount when an executor tool is enabled: tighter when unused, but adds branching and a divergent layout. Rejected for consistency.
- Make the subdir name/path configurable: no demonstrated need; the example fixes it to `tools`. Rejected (can be added later).

## Risks / Trade-offs

- **[Forced `HOME` changes `$HOME` value for all sandboxed commands]** → Today `$HOME` points at an unmounted host path (effectively broken); forcing `/home/blowball` is strictly more functional. A command that persisted state under the *host* home inside the sandbox could not have worked anyway. No real regression; documented in proposal.
- **[`getpwuid()`-based tools ignore `$HOME` env]** → A tool reading home via the passwd db (`/etc/passwd`, bound RO from host) sees the server uid's host home, not `/home/blowball`. Mitigation: defer a synthetic `/etc/passwd` override to a future change; most tools/shells honor `$HOME`. Tracked as a known limitation.
- **[tmpfs memory pressure]** → A pathological command writing large files to `$HOME` consumes namespace memory (default tmpfs size). Mitigation: bounded by bwrap's default tmpfs limit and the existing per-command timeout; workspace writes still go to `/workspace` (host-backed). Acceptable for an execution sandbox.
- **[RO landlock on `tools` requires helper refactor]** → Touches `landlock_linux.go`/`landlock_other.go` + signature. Contained; covered by the existing rotation test plus a new RO scenario.
- **[Operator tool malicious content]** → Tools run with the same isolation as any sandboxed command (network-off by default, workspace-scoped, uid-mapped). RO mount prevents the sandbox from modifying the operator's tools. No new exposure vs. today's `/usr` RO bind.
- **[Empty tools dir still reshapes `HOME`/`PATH`]** → Adds an empty `$HOME/.local/bin` to `PATH` and forces `/home/blowball` even when unused. Harmless and uniform; documented.

## Migration Plan

- **Deploy:** no schema/config migration. Operators who want tools place binaries in `{data-dir}/tools`; operators who do not are unaffected (empty dir, harmless extra `PATH` entry, forced synthetic `HOME`).
- **Rollback:** revert the change; the sandbox returns to leaking host `$HOME` and omitting the tools mount. No persisted state depends on the new behavior (tmpfs `$HOME` is ephemeral by design).
- **Ordering:** landlock signature change, `NewTools`/`buildBwrapArgs` arg additions, and `serve.go` wiring land together in one change (no intermediate broken state).

## Open Questions

- **Synthetic `/etc/passwd`:** defer unless a concrete operator tool resolves home via `getpwuid()` and fails. If needed, a future change binds a generated `/etc/passwd` with the chosen `HOME` for the sandbox uid.
- **`HOME` path naming:** `/home/blowball` is chosen for clarity; not configurable now. If deployments need a specific value (e.g. to match an existing uid), surface as config later.
- **PATH order vs. host tools:** prepend is the default (operator tools win). If an operator wants host tools to take precedence, that becomes a future config knob; not needed now.
