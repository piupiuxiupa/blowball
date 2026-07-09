## Context

Blowball already has sandboxed `bash` and `python` executor tools in `internal/tool/executor/`. They run inside a bubblewrap sandbox with `/workspace` bound to the user's workspace, `/usr`/`/lib`/`/etc` read-only, optional network isolation, and filtered environment variables. The `python` tool has no way to install missing packages because the sandbox has no network by default and system site-packages are read-only.

## Goals / Non-Goals

**Goals:**
- Let agents install Python packages when code fails with `ModuleNotFoundError` / `ImportError`.
- Persist installed packages in the user's workspace so they survive across agent rounds and sessions.
- Make installed packages automatically available to the existing `python` tool without requiring the agent to manipulate `sys.path`.
- Allow operators to configure a PyPI mirror / index URL in `config.yaml`.
- Reuse the existing executor infrastructure (bwrap, audit logging, config loading, tool registry).

**Non-Goals:**
- Full virtual-environment management (venv/conda).
- Cross-session package sharing between users.
- Resolving package version conflicts beyond normal pip semantics.
- Support for platforms other than Linux (executor tools are already Linux-only).

## Decisions

### 1. Install packages with `pip install --target /workspace/.pip`

**Rationale:**
- `/usr` is read-only inside the sandbox, so installing into system site-packages fails.
- `pip install --user` writes to `~/.local`, but the sandbox does not bind the host home directory, so the target path may not exist or be writable.
- `--target /workspace/.pip` writes directly into the user's workspace, which is persistent and already scoped per-user.

**Alternatives considered:**
- `--user`: rejected because `HOME` is not reliably writable inside the sandbox.
- `python -m venv /workspace/.venv`: more complete isolation, but requires activating the venv for every `python` tool invocation and complicates the `python` tool's command construction.

### 2. Expose installed packages via `PYTHONPATH`

**Rationale:**
- The `python` tool runs as `python3 -c <code>` or `python3 /workspace/<file>`. If `PYTHONPATH=/workspace/.pip` is injected into the sandbox environment, imports work transparently.
- This keeps the agent-facing API unchanged; the agent does not need to prepend paths in its code.

**Implementation:**
- The bwrap argument builder adds `--setenv PYTHONPATH /workspace/.pip` whenever `PYTHONPATH` is not already in the allowed environment patterns, or appends `/workspace/.pip` if it is.
- The `pip_install` tool also needs `PYTHONPATH` set so that pip can see already-installed dependencies during incremental installs.

### 3. Add a dedicated `pip_install` tool instead of reusing `bash`

**Rationale:**
- A named tool with a clear description (`"Use this tool when Python code fails with ModuleNotFoundError..."`) makes the model more likely to invoke it at the right moment.
- A dedicated tool can enforce safe defaults: `--target /workspace/.pip`, configured mirror, audit logging, and timeout.
- The `bash` tool is not granted to all agents by default.

### 4. Place configuration under `tools.executor.pip`

**Rationale:**
- `pip_install` is part of the executor family; it shares timeout, output limit, network, and environment-pattern semantics with `bash` and `python`.
- A nested `pip` block keeps the config hierarchy consistent.

**Configuration fields:**
- `enabled` (bool)
- `timeout` (duration) — default `120s` because installs can be slow
- `max_output_bytes` (int)
- `allowed_env_patterns` ([]string)
- `network` (bool) — default `true` because pip requires network
- `index_url` (string) — PyPI mirror, e.g. `https://pypi.tuna.tsinghua.edu.cn/simple`
- `extra_index_urls` ([]string)
- `trusted_hosts` ([]string) — passed as `--trusted-host` for HTTP mirrors or self-signed HTTPS

### 5. Network default is `true` for `pip_install`

**Rationale:**
- pip is useless without network access in most cases.
- This differs from `bash`/`python` (default `false`), so it must be clearly documented.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Arbitrary code execution during package builds | Same mitigation as `bash`/`python`: runs inside bwrap with filesystem/network isolation and no privileges. |
| Package installs can exceed the default 30s timeout | Default timeout for `pip_install` is 120s and configurable. |
| Large packages (e.g. `torch`) can exceed `max_output_bytes` | Output is pip logs, not the package itself; `max_output_bytes` default of 65536 is reasonable. Keep configurable. |
| macOS/Windows developers cannot test this locally | Same as existing executor tools; integration tests run only on Linux or are skipped. |
| Package version conflicts between user-installed and system packages | Same as normal pip `--target` behavior; acceptable limitation. |
| Agent may try to install malicious packages | No mitigation beyond sandboxing and audit logging; same trust model as `bash`/`python`. |

## Migration Plan

- No database migration or data migration required.
- Operators who want the tool must opt in by setting `tools.executor.pip.enabled: true` and adding `pip_install` to agent tool lists.
- Default config keeps the tool disabled, so existing deployments are unaffected.

## Open Questions

- Should `pip_install` also accept a `requirements` file path parameter? This is useful but can be added later without breaking changes.
- Should the target directory be `.pip` or something more explicit like `.blowball-pip-packages`? Using `.pip` is short and conventional.
