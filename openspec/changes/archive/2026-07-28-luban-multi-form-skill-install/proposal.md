## Why

`luban_install_skill` today decides how to install purely from the URL suffix: a URL ending in `.md`/`SKILL.md` is downloaded as a single skill file, everything else is `git clone --depth 1`. That covers two shapes and nothing else, so two real-world install intents fail:

1. **Selecting one skill from a collection repo.** A request like `npm skills add https://xxxx.git --skill xxxx` (or the natural-language equivalent "install only `X` from that repo") has no expression: the tool always installs the whole repo as one entry and only honors a root `SKILL.md`.
2. **An install-documentation URL that is not itself a skill.** A request like "请根据 `https://skillhub.cn/install/skillhub.md` 安装 `gildata-finance-data`" points luban at a *recipe*, not the skill. Today luban either errors opaquely (`downloaded file is not a valid SKILL.md`) or, if the doc happens to carry frontmatter, installs the wrong content under the requested name.

The model is the right orchestrator for "fetch the doc → read it → install the real source it points at" — that is exactly the multi-step tool-calling Confucius already does. Luban's job is to expose composable, terminal install shapes and to *hand back* a document it cannot install, instead of dead-ending.

## What Changes

- `luban_install_skill` gains an optional **`skill`** parameter. For git-repo sources it clones the collection, then installs only the selected sub-skill (matched by frontmatter `name`, else by repo-relative subpath) as its own entry; the rest of the clone is discarded. This realizes the `--skill <name>` intent.
- The single-file (`.md`) download path changes from "error if not a valid SKILL.md" to **"return the fetched content as an install document"**: when the body is not a valid SKILL.md, the tool returns a structured, non-error result carrying the document text and a hint to read it and re-install from the real source. This lets the agent continue the install in-loop without a separate fetch round-trip.
- The `luban_install_skill` tool description and the system-prompt Skills section are updated to describe the supported install shapes (whole repo, repo + sub-skill selection, single SKILL.md, install-documentation return) and the agent-orchestrated manifest flow.
- `webfetch` is unchanged; the agent may still use it to pre-read a doc. Luban's install-doc return simply removes the forced extra round-trip when the agent already pointed luban at the doc URL.

## Capabilities

### Modified Capabilities

- `luban-skill-tools`: `luban_install_skill` gains the `skill` sub-skill selection parameter and the install-documentation return behavior (both added as new requirements; the existing install requirement is untouched).
- `system-prompt-rendering`: the Skills section gains guidance describing the supported install shapes and instructing the agent to follow install-documentation content to the real source.

## Impact

- `internal/tool/luban/install.go`: `installSkill` accepts a `skill` argument; new sub-skill selection logic after a clone; `installSingleFile` returns an install-doc result instead of erroring on non-skill content; result type becomes discriminated (`installed` vs `install-doc`).
- `internal/tool/luban/register.go`: `luban_install_skill` parameter schema adds `skill`; tool description updated.
- `internal/tool/luban/luban_test.go`: cover sub-skill selection (by name and subpath, including not-found listing) and the install-doc return.
- `internal/prompt/render.go` + `internal/agent/orchestrator.go`: Skills-section install guidance.
- No changes to `webfetch`, the skill loader discovery, or where skills are stored (storage location is owned by the separate `relocate-user-skills-to-workspace` change).
