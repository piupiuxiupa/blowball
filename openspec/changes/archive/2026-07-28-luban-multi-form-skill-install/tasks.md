## 1. Sub-skill selection (`skill` parameter)

- [x] 1.1 In `internal/tool/luban/install.go`, extend `installSkill` to accept a `skill` argument and thread it through `register.go`'s Execute handler
- [x] 1.2 Add a `installSubSkill` path for git-repo sources: clone to a staging dir, discover sub-skills (reuse loader discovery), match `skill` by frontmatter `name`; fall back to repo-relative subpath containing `SKILL.md`
- [x] 1.3 Move only the selected sub-skill directory into `{userSkills}/{installed-name}/`; remove the rest of the clone; honor the existing `name` override for the installed dir
- [x] 1.4 On no/multiple match, return an error listing the discovered sub-skill names
- [x] 1.5 For single-file sources with `skill` set, proceed with the single-file install and note in the result that `skill` is ignored for single files

## 2. Install-documentation return

- [x] 2.1 In `installSingleFile`, when the fetched body is not a valid SKILL.md (frontmatter missing/invalid), return a structured non-error result (`installed:false`, `kind:"install-doc"`, `url`, `content`, `hint`) instead of erroring
- [x] 2.2 Make the install result type discriminated (`installed` install vs `install-doc`) so the registry handler can return it directly
- [x] 2.3 Ensure HTTP non-200 / non-text responses still return an error (only HTTP 200 text bodies become install-docs)

## 3. Tool registration + description

- [x] 3.1 In `internal/tool/luban/register.go`, add the optional `skill` parameter to the `luban_install_skill` JSON schema
- [x] 3.2 Update the `luban_install_skill` tool description to list the supported shapes (whole repo, repo + `skill` selection, single SKILL.md, install-doc return) and the manifest flow
- [x] 3.3 Decode `skill` from args and pass it into `installSkill`

## 4. System prompt guidance

- [x] 4.1 In `internal/prompt/render.go` Skills section, add guidance: describe the install shapes; instruct the agent that a non-skill `.md` URL returns its content as an install doc to read and follow to the real source; mention `skill` for selecting one sub-skill from a collection
- [x] 4.2 Update `internal/prompt/render_test.go` to assert the new install guidance is present

## 5. Verification

- [x] 5.1 Add/extend `internal/tool/luban/luban_test.go`: sub-skill selection by name, by subpath, not-found listing, `name` override; install-doc return for a non-skill `.md`; single-file install still works for a valid SKILL.md
- [x] 5.2 `make test` and `go test ./test/integration/...` — confirm no regressions in luban tool registration, install, and orchestration
- [x] 5.3 Manual: simulate "请根据 <skillhub.md URL> 安装 X" end-to-end — agent receives the install-doc, reads it, and installs the real source
