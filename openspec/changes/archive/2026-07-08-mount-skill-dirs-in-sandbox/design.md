## Context

Blowball exposes skills to agents through two mechanisms:

1. **System prompt injection** — allowed global skills are listed in the system prompt, and agents use `luban_read_skill` to load `SKILL.md` content.
2. **Sandboxed execution** — agents use `bash` and `python` tools, which run inside a bubblewrap sandbox whose filesystem only contains `/workspace` (bound to `data/{userID}/workspace`) plus a minimal set of host system directories.

Some skills, such as `ifind-finance-data`, include helper scripts (`call-node.js`, `call.py`) and configuration files (`mcp_config.json`) alongside `SKILL.md`. The instructions in `SKILL.md` assume the agent can locate and execute these files. Today the agent cannot, because:

- The system prompt exposes only the per-user skills directory (`data/{userID}/skills`), not the global skills directory (`skills/`).
- `xizhi_*` tools are scoped to the workspace and explicitly forbidden from accessing skill directories.
- The bubblewrap sandbox does not mount any skill directory, so even a correctly-constructed path like `node skills/ifind-finance-data/call-node.js` fails inside the sandbox.

## Goals / Non-Goals

**Goals:**

- Make the global skills directory and per-user skills directory visible inside the `bash`/`python` sandbox as read-only mounts.
- Update the system prompt to expose both directories using sandbox-resolvable paths (`/skills/global`, `/skills/user`).
- Clarify in the system prompt that agents may read and execute skill files using `bash`/`python` tools, while skill directories remain read-only.
- Update the `ifind-finance-data` skill documentation to use sandbox-resolvable paths.

**Non-Goals:**

- Changing how `luban_read_skill` works or adding new skill-management tools.
- Allowing agents to write or modify files in skill directories.
- Supporting skill directory access on non-Linux platforms where executor tools are disabled.
- Replacing bubblewrap with a different sandbox technology.

## Decisions

### Use two fixed sandbox mount points: `/skills/global` and `/skills/user`

**Rationale:** A single mount point would require merging global and user skills, which complicates override semantics and symlink handling. Two fixed paths mirror the existing two-source skill loader (`Loader.ListGlobal` and `listUser`) and keep the implementation simple.

**Alternative considered:** Merge both directories into one `/skills` mount. Rejected because user skills override global skills by name, and a merged view would need to resolve conflicts at mount time or rely on overlay filesystems, adding unnecessary complexity.

### Expose sandbox paths in the system prompt, not host paths

**Rationale:** The agent consumes paths through the `bash`/`python` tools, which only understand the sandbox filesystem. Telling the agent about host paths (`skills/`, `data/{userID}/skills/`) would cause commands to fail inside the sandbox.

**Alternative considered:** Expose both host and sandbox paths. Rejected because it adds noise and the agent has no way to use host paths.

### Mount skill directories read-only

**Rationale:** Skills are declarative instructions and shared resources. Allowing agents to mutate them would break isolation and reproducibility.

**Implementation:** Use bubblewrap `--ro-bind` for both `/skills/global` and `/skills/user`.

### Keep `xizhi_*` tools forbidden for skill directories, but allow `bash`/`python` reads

**Rationale:** `xizhi_*` tools are designed for the workspace and apply workspace-specific validation. Skill files should be accessed through the same execution channel used to run them (`bash`/`python`). The prompt will be updated from a blanket prohibition to a more precise rule: do not modify skill directories with `xizhi_*` tools.

### Pass skill directories through the executor tool constructor, not context

**Rationale:** The existing executor tool closures capture configuration at registration time, matching the pattern used for `workspaceRootForUser`. This avoids threading new context values through the tool execution path.

## Risks / Trade-offs

- **[Risk]** Agent confuses workspace paths with skill paths and tries to write skill output to `/skills/global`.  
  **Mitigation:** `--ro-bind` ensures writes fail; the prompt explicitly states skill directories are read-only.

- **[Risk]** A skill with a malicious helper script gains network access through the sandbox.  
  **Mitigation:** Network isolation (`--unshare-net` by default) and dangerous-command detection remain unchanged. Skill scripts run with the same restrictions as any other sandboxed command.

- **[Risk]** macOS/Windows developers see prompt references to `/skills/global` but executor tools are disabled, leading to confusing failures if an agent tries to execute a skill script.  
  **Mitigation:** This is acceptable because skills that require local execution are inherently Linux-only when executor tools are disabled. The `ifind-finance-data` skill already requires Node.js or Python, which are only available inside the sandbox on Linux. Documentation will note this limitation.

- **[Risk]** Skills with large `references/` directories increase the sandbox bind-mount surface.  
  **Mitigation:** Read-only bind mounts do not copy data; they only add kernel mount entries. Performance impact is negligible unless the number of mounts becomes very large.

## Migration Plan

1. Merge the implementation change.
2. Update `skills/ifind-finance-data/SKILL.md` to reference `/skills/global/ifind-finance-data/...` paths.
3. On existing deployments, restart the server to pick up the new tool registration and prompt rendering.
4. No database or filesystem migration is required.

## Open Questions

- Should user skills be installed into the workspace automatically when an agent needs them, or is the per-user `data/{userID}/skills/` mount sufficient?  
  **Current decision:** The per-user mount is sufficient; `luban_install_skill` already writes there.
- Should the system prompt expose the host path in addition to the sandbox path for debugging?  
  **Current decision:** No; only sandbox paths are actionable for the agent.
