## Context

Per-user skills currently live at `data/{userID}/skills/`, a sibling of `data/{userID}/workspace/`. Three things depend on that placement:

1. `xizhi_*` tools are scoped to `…/workspace/` and physically cannot reach `…/skills/`, which is the only thing enforcing "skills are not touched by file tools".
2. The executor sandbox `--ro-bind`s `…/skills/` to a separate `/skills/user` mount so bash/python can run skill-shipped helper scripts read-only.
3. The system prompt tells the agent the per-user skills directory is `/skills/user`.

The user wants per-user skills to live *inside* the workspace as a hidden `.blowball/skills/` directory, and to keep the "skills are managed only via luban" model.

Moving the directory under the workspace breaks assumption (1): `xizhi_*` would suddenly be able to read and write skills, because `validatePath` only blocks absolute paths, `..`, and symlink escapes — not workspace-internal subdirectories. So the separation must be re-established by *reserving* `.blowball` in path validation.

## Goals / Non-Goals

**Goals:**
- Per-user skills live at `data/{userID}/workspace/.blowball/skills/`.
- Skills remain manageable only through `luban_*` tools: `xizhi_*` cannot reach `.blowball`.
- `.blowball` is hidden from workspace listings and reserved as a namespace for future `.blowball/*` content.
- Sandbox access to skill-shipped helper scripts keeps working; global skills stay read-only at `/skills/global`.

**Non-Goals:**
- Data migration of existing `data/{userID}/skills/` trees (operator handles separately).
- Changing *global* skills location (`{data-dir}/skills/`) — only per-user skills move.
- Restricting the executor (bash/python) from writing under `.blowball`. Arbitrary sandboxed execution can write anywhere under `/workspace`; the reservation targets the file tools, where the realistic model-confusion risk lives.
- Changing luban install *behavior* (that is a separate change: `luban-multi-form-skill-install`).

## Decisions

### 1. Reserve `.blowball` in `xizhi` path validation (keep the hard boundary by path, not physical separation)
- **Rationale**: The user wants skills under the workspace but still "只走 luban". `validatePath` already centralizes the workspace-boundary check; adding a reserved-segment rule (`cleaned` first segment == `.blowball` → reject) reproduces today's "xizhi cannot touch skills" guarantee without keeping skills on a separate branch of the tree. Same error class (`ErrPathOutsideWorkspace`-style guidance) so the model self-corrects.
- **Alternative considered**: Leave `.blowball` writable by `xizhi_*` and let skills be first-class workspace files. Rejected — contradicts "只走 luban" and the existing luban/xizhi split that the prompt and tool descriptions already encode.

### 2. Drop the `/skills/user` sandbox mount; expose per-user skills at `/workspace/.blowball/skills`
- **Rationale**: Once skills live under the workspace, `/workspace` (read-write) already contains `.blowball/skills`. A second `--ro-bind` of the same host directory at `/skills/user` would be cosmetic — the agent could still write via `/workspace/.blowball/skills`. Mounting the same tree twice is confusing and gives a false read-only impression. The honest model is: **global** skills read-only at `/skills/global`; **per-user** skills at `/workspace/.blowball/skills` (read-write, owned by the user, managed via luban).
- **Alternative considered**: Keep an `--ro-bind …/workspace/.blowball/skills /skills/user` so user skills have a clean read-only path for executing helper scripts. Rejected as misleading; the read-write copy at `/workspace/.blowball/skills` is the real surface.

### 3. `.blowball` is a reserved namespace, skills at `.blowball/skills`
- **Rationale**: Naming the dir `.blowball` (not `.skills`) leaves room for future operator/application state under the workspace (e.g. `.blowball/config`, `.blowball/cache`) without colliding with user files. The hidden-dotfile convention (`isHiddenName`) already keeps it out of listings; this change promotes that to a firm requirement for reserved workspace-internal directories.

### 4. Plumb the new path through `fs.Store.UserSkills` so wiring follows automatically
- **Rationale**: `cmd/blowball/serve.go`, the luban bundle, the executor bundle, `SkillHandler`, and the orchestrator all resolve the per-user skills dir via `fsStore.UserSkills(userID)` (directly or through a closure). Changing that one method's return value propagates everywhere; no call-site edits needed except removing the executor's dropped mount.

## Risks / Trade-offs

- **[Risk] Per-user skills become writable inside the executor sandbox** → **Mitigation**: Acceptable. Per-user skills are owned by the authenticated user and already mutable via `luban_install_skill`; bash writing under `.blowball` is not a privilege escalation. The reservation on `xizhi_*` covers the model-confusion path. Global skills remain read-only.
- **[Risk] Existing skill docs / agent habits reference `/skills/user/...`** → **Mitigation**: Only *global* skill docs ship in-tree and reference `/skills/global/...` (unchanged). Per-user skills are agent-installed at runtime; the system prompt gives the agent the new path, so no shipped doc references `/skills/user`.
- **[Risk] Reservation is patch-width: only `.blowball` is reserved, not arbitrary dotfiles** → **Mitigation**: Intentional. Users may legitimately keep dotfiles in their workspace; reserving only the application namespace avoids surprising them.
- **[Risk] `EnsureUserDirs` must create a nested `.blowball/skills`** → **Mitigation**: Trivial `MkdirAll` under the workspace; idempotent.

## Relationship to Other Changes

- **`luban-multi-form-skill-install`** (separate change) adds install behavior to `luban-skill-tools` and guidance to `system-prompt-rendering` as *new* requirements, so it does not collide with this change's path updates. The two are independent and may land in either order; once both land, the merged `luban-skill-tools` capability has the new path *and* the new install shapes.
