## Why

Skills such as `ifind-finance-data` ship with helper scripts (`call-node.js`, `call.py`) and configuration files (`mcp_config.json`) inside the skill directory. The agent currently cannot read or execute these files because the bash/python sandbox only mounts the user's workspace, and the system prompt only exposes the per-user skills directory. This change makes both the global and per-user skill directories visible inside the execution sandbox and tells the agent where to find them.

## What Changes

- Update the system prompt to expose two read-only skill directories:
  - Global skills directory (`skills/` on the host, mounted at `/skills/global` in the sandbox)
  - Per-user skills directory (`data/{userID}/skills/` on the host, mounted at `/skills/user` in the sandbox)
- Relax the current blanket prohibition on using `xizhi_*` tools against the skills directory so agents can read skill files with `bash`/`python` tools; the directories remain read-only and cannot be modified.
- Mount both skill directories read-only inside the bubblewrap sandbox used by the `bash` and `python` executor tools.
- Update the `ifind-finance-data` skill documentation to reference sandbox-resolvable paths (e.g. `/skills/global/ifind-finance-data/call-node.js`) instead of host-relative paths.

## Capabilities

### New Capabilities

- `skill-directory-sandbox-access`: Agents can read and execute helper scripts and configuration files located in global and per-user skill directories from within the sandboxed execution environment.

### Modified Capabilities

- `system-prompt-rendering`: The system prompt must render both global and per-user skill directory paths, and clarify that `bash`/`python` may be used to read skill files while `xizhi_*` must still not be used to modify skill directories.
- `executor-tools`: The `bash` and `python` tools must accept and mount global and per-user skill directories as read-only paths inside the bubblewrap sandbox.

## Impact

- `internal/prompt/render.go` and `internal/agent/orchestrator.go`: prompt rendering and orchestrator wiring.
- `internal/tool/executor/*`: bubblewrap argument construction, tool construction, and registration.
- `cmd/server/main.go`: pass skill directories into the executor tool bundle.
- `internal/handler/session.go`: already passes per-user skill directory; no change unless path calculation is centralized.
- `skills/ifind-finance-data/SKILL.md`: update script and references paths.
- macOS and Windows development are unaffected because executor tools are only enabled on Linux with bubblewrap; the prompt changes apply everywhere.
