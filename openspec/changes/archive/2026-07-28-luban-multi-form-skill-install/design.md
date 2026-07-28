## Context

`luban_install_skill` (`internal/tool/luban/install.go`) branches on `isSingleSkillURL(u.Path)`:

- URL ends in `.md`/`SKILL.md` → `installSingleFile`: download (≤500KB), parse frontmatter; **error** if no valid `name`+`description`; else write `{userSkills}/{name}/SKILL.md`.
- otherwise → `installGitRepo`: `git clone --depth 1` into `{userSkills}/{name}`; optionally rename to the root SKILL.md frontmatter name.

Two intents have no expression:

- **Sub-skill selection**: a repo is a *collection* (`{repo}/skills/{a}`, `{repo}/skills/{b}`, …). The user wants only `--skill b`. The loader's recursive discovery already finds these post-clone, but install always lands the whole repo under one name.
- **Install documentation**: `skillhub.md` is prose that tells you where the real skill lives. Luban can't install prose; today it dead-ends with `"downloaded file is not a valid SKILL.md"`.

The user's stated preference is an **agent-orchestrated** flow: the model fetches/reads the doc, decides the real source, then installs. That keeps luban small and composable and matches blowball's existing Confucius tool-calling loop.

## Goals / Non-Goals

**Goals:**
- `luban_install_skill` can install a single named sub-skill from a cloned collection (`skill` parameter).
- `luban_install_skill` returns install-documentation content (instead of an opaque error) when a `.md` URL is not a valid skill, so the agent can read it and continue.
- The tool description and system prompt describe the install shapes and the manifest flow clearly enough that the agent reliably chooses the right one.

**Non-Goals:**
- Building a server-side manifest *interpreter* (parsing a structured install format and auto-following it). The agent reads the doc; luban only returns it. A structured-manifest fast-path can be added later if publishers adopt a format.
- Supporting archive sources (zip/tar), raw-file GitHub URLs, or git branch/tag selection. Open for future work; not required by the cited flows.
- Changing where skills are stored or how they are discovered/listed/read.
- Changing `webfetch`.

## Decisions

### 1. Agent orchestrates the multi-step install; luban stays a composable terminal primitive
- **Rationale**: "Read a doc at a URL and figure out what to install" is exactly what a tool-calling agent is good at and what a URL-suffix branch in luban is bad at. Keeping luban's logic to well-defined terminal shapes (clone / clone+select / single-file / return-doc) preserves the existing architecture (Confucius as orchestrator) and avoids a brittle server-side doc parser.
- **Alternative considered (rejected)**: Teach luban to parse a manifest format and auto-follow it (e.g. a `source:` field in frontmatter). More powerful for publishers who adopt the format, but useless for human-prose install docs like the skillhub example, and it grows a format-specific interpreter we would have to maintain. Can be layered on later as an optional fast-path.

### 2. `skill` parameter: select one sub-skill from a cloned collection
- **Semantics** (applies to git-repo sources only; ignored for single-file):
  1. Clone the repo to a temp/staging location.
  2. Discover sub-skills (recursive `SKILL.md`, reusing the loader's discovery).
  3. Match `skill` to a discovered skill by frontmatter `name`; if exactly one matches, select it.
  4. Else treat `skill` as a repo-relative subpath; if that dir contains a `SKILL.md`, select it.
  5. Else return an error listing the discovered sub-skill names so the agent can retry with a valid value.
  6. Move only the selected sub-skill's directory into `{userSkills}/{installed-name}/` and remove the rest of the clone.
- **Installed name**: the selected skill's frontmatter `name` by default; overridable by the existing `name` parameter. `skill` selects *which*; `name` renames *the target dir*.
- **Rationale**: frontmatter-`name` match is the least surprising for skill authors; subpath match covers repos whose skills are not named in frontmatter. Listing available names on failure turns a dead-end into a self-correcting loop.

### 3. Install-documentation return on non-skill `.md`
- **Semantics**: on the single-file path, if the fetched body is not a valid SKILL.md (no frontmatter, or frontmatter missing `name`/`description`), do **not** error. Return a structured, non-error result:
  - `installed: false`
  - `kind: "install-doc"`
  - `url`, `content` (the fetched markdown text)
  - `hint`: a short instruction to read the content, find the real source, and call `luban_install_skill` again with that URL (and an optional `skill`).
- **Disambiguation is content-based, not URL-based**: a `.md` with valid skill frontmatter is still installed as a skill (existing behavior). Frontmatter presence remains the "is it a skill" signal. This means a meta-skill doc that carries frontmatter will install as-is — acceptable, because the agent can then `luban_read_skill` it.
- **Rationale**: the agent is already in a tool-calling loop; handing it the doc text lets it continue without a redundant `webfetch`. Returning content for a genuinely broken fetch (e.g. an HTML 404 page) is harmless — the agent reads it and sees the error.

### 4. Purely additive spec deltas — no collision with the relocation change
- `luban-skill-tools` and `system-prompt-rendering` are touched by both this change and `relocate-user-skills-to-workspace`. To avoid edit conflicts, this change only **adds** new requirements (sub-skill selection, install-doc handling, install guidance); it does not rewrite the path-bearing install/scoping requirements that the relocation change modifies. The two land independently in either order.

## Risks / Trade-offs

- **[Risk] Sub-skill match ambiguity** (multiple skills share a frontmatter name) → **Mitigation**: selection requires exactly one name match; if more than one, fall through to subpath match, else error listing the candidates.
- **[Risk] `skill` silently ignored on a single-file URL** → **Mitigation**: document explicitly that `skill` applies to repo sources; on a single-file install with `skill` set, proceed with the single-file install and include a note in the result.
- **[Risk] Install-doc return masks a real download failure** (e.g. an error page without frontmatter) → **Mitigation**: the result includes the raw `content` and the response is only treated as a doc on HTTP 200 with a text content type; the agent reads the content and self-corrects.
- **[Risk] Larger repo clones just to select one skill** → **Mitigation**: `--depth 1` is already used; selection discards the rest. Acceptable for typical skill collections.

## Open Questions

- Should `skill` also accept a glob/pattern, or is exact name + subpath sufficient? (Default: exact only.)
- Should the install-doc return cap `content` length (e.g. the existing 500KB download cap already bounds it)?
