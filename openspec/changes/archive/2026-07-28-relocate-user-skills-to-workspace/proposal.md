## Why

Per-user skills today live at `data/{userID}/skills/` — a sibling of the user's `workspace/`. That puts them outside the workspace tree, so they do not travel with the workspace, are not addressed by workspace-relative tooling, and require a separate sandbox mount (`/skills/user`) and a separate REST handler path just to be visible. The agent is told skills are off-limits to `xizhi_*` only by prompt convention, backed by physical separation.

Moving per-user skills into the workspace as a hidden directory — `data/{userID}/workspace/.blowball/skills/` — makes "everything a user owns lives under their workspace" true, removes the separate sandbox mount, and lets the existing dotfile-hiding convention keep the directory out of listings. The skills/`xizhi_*` separation is preserved by *reserving* the `.blowball` name in path validation rather than by physical separation, so the agent still manages skills only through `luban_*` tools.

## What Changes

- Relocate the per-user skills directory from `data/{userID}/skills/` to `data/{userID}/workspace/.blowball/skills/`. `.blowball/` is a reserved, hidden namespace under the workspace (room for future `.blowball/*` content; skills live at `.blowball/skills`).
- Reserve `.blowball` in `xizhi` path validation: any `xizhi_*` read/write/modify/list/tree/glob path whose first segment is `.blowball` is rejected, so skills stay manageable only via `luban_*` tools (the current "never use xizhi on skills" rule becomes enforced by path, not just prompt).
- Drop the executor sandbox's `--ro-bind {userSkillsDir} /skills/user` mount. Per-user skills are now visible inside the sandbox at `/workspace/.blowball/skills/` (part of the existing read-write `/workspace` bind). The global skills mount `/skills/global` (read-only) is unchanged.
- Update the system prompt: the per-user skills directory is exposed as `/workspace/.blowball/skills`; the Skills section clarifies that **global** skill directories are read-only, while per-user skills live under the workspace and are managed via `luban_*` tools.
- `.blowball` is excluded from workspace file listings (already handled by the existing `isHiddenName` dotfile rule; this change makes the exclusion a firm requirement for reserved workspace-internal directories, not just a default).

## Capabilities

### New Capabilities

- `user-skills-workspace-layout`: Defines the canonical on-disk location of per-user skills (`data/{userID}/workspace/.blowball/skills/`), the reserved `.blowball` namespace under the workspace, and the rule that reserved workspace-internal directories are never surfaced in workspace listings.

### Modified Capabilities

- `xizhi-tools`: Path validation MUST reject the reserved `.blowball` workspace-internal directory for all `xizhi_*` operations.
- `skill-directory-sandbox-access`: Per-user skills are no longer mounted at a separate `/skills/user` read-only path; they are reached at `/workspace/.blowball/skills/` inside the sandbox. Global skills remain at `/skills/global` (read-only).
- `system-prompt-rendering`: The environment section exposes the per-user skills directory as `/workspace/.blowball/skills`; the Skills section distinguishes read-only global skill directories from workspace-resident per-user skills managed via luban.
- `luban-skill-tools`: Install and security-scoping requirements reference the new per-user skills path `data/{userID}/workspace/.blowball/skills/`.

## Impact

- `internal/store/fs/user.go`: `UserSkills` returns `…/workspace/.blowball/skills`; `userSubDirs` no longer creates a top-level `skills/`; `EnsureUserDirs` creates `.blowball/skills` under the workspace.
- `internal/tool/xizhi/validate.go`: `validatePath` rejects a cleaned path whose first segment is `.blowball`.
- `internal/tool/executor/bwrap.go` + `executor.go`: drop the `/skills/user` mount and the now-unused per-user skills resolver from the executor bundle.
- `internal/prompt/render.go` + `internal/agent/orchestrator.go`: per-user skills directory path and Skills-section guidance.
- `internal/handler/skill.go` and `cmd/blowball/serve.go`: follow `UserSkills` automatically; no semantic change beyond the path.
- Shared-storage (`storage.workspace.backend: shared`) is unaffected — per-user skills already live under the shared `data/{userID}` tree; only their sub-location changes.
- Data migration of existing `data/{userID}/skills/` content is **out of scope** for this change (handled separately by the operator).
