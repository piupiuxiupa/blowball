## 1. Per-user skills path

- [x] 1.1 In `internal/store/fs/user.go`, change `UserSkills(userID)` to return `filepath.Join(s.userDir(userID), "workspace", ".blowball", "skills")`
- [x] 1.2 Update `userSubDirs` to remove the top-level `"skills"` entry (keep `sessions`, `workspace`); in `EnsureUserDirs`, create `.blowball/skills` beneath the workspace after the workspace dir exists
- [x] 1.3 Update `internal/store/fs/fs.go` doc comment (the on-disk layout block) to reflect `workspace/.blowball/skills/`
- [x] 1.4 Add/update `internal/store/fs` tests asserting `UserSkills` resolves under the workspace and that `EnsureUserDirs` creates `.blowball/skills`

## 2. Reserve `.blowball` in xizhi path validation

- [x] 2.1 In `internal/tool/xizhi/validate.go` `validatePath`, after `filepath.Clean`, reject any path whose first segment is `.blowball` (return the same outside-workspace error style with guidance)
- [x] 2.2 Add cases to `internal/tool/xizhi/validate_test.go`: `.blowball/skills/x`, `.blowball/anything`, and that a non-reserved dotfile (e.g. `.env`) is still allowed
- [x] 2.3 Confirm `WorkspaceHandler` read/write/list paths inherit the reservation via `xizhi.ValidatePath` (no separate change expected)

## 3. Sandbox: drop `/skills/user` mount

- [x] 3.1 In `internal/tool/executor/bwrap.go` `buildBwrapArgs`, remove the `--ro-bind {userSkillsDir} /skills/user` line; keep `--ro-bind {globalSkillsDir} /skills/global`
- [x] 3.2 Drop the now-unused `userSkillsDir` parameter from `buildBwrapArgs` and update its callers
- [x] 3.3 In `internal/tool/executor/executor.go`, remove the `userSkillsFn`/`userSkillsDir` plumbing from `Tools`/`NewTools` if no longer used; keep `globalSkillsDir`
- [x] 3.4 Update `internal/tool/executor/bwrap_test.go` to assert `/skills/user` is absent and `/skills/global` is present
- [x] 3.5 Update `cmd/blowball/serve.go` `executor.NewTools` call site to match the new signature

## 4. System prompt

- [x] 4.1 In `internal/agent/orchestrator.go` `renderSystemPrompt`, change the per-user skills directory passed to the prompt from `/skills/user` to `/workspace/.blowball/skills`
- [x] 4.2 In `internal/prompt/render.go` skills section, clarify: global skill directories are read-only; per-user skills live under the workspace at `.blowball/skills` and are managed via luban; `xizhi_*` must not access `.blowball`
- [x] 4.3 Update `internal/prompt/render_test.go` assertions for the new per-user skills path and the revised guidance text

## 5. Verification

- [x] 5.1 `make test` — fix regressions in fs store, xizhi validation, executor bwrap args, and prompt rendering
- [x] 5.2 `go test ./test/integration/...` — confirm skill listing, luban install/read, and orchestration still resolve skills under the new path
- [x] 5.3 On Linux with bwrap: confirm a sandboxed `bash` can read `/workspace/.blowball/skills/...` and `/skills/global/...`, and that `/skills/user` no longer exists — encoded as the linux-only `TestExecutorSkillDirectoryAccess` (reads both paths, asserts `/skills/user` missing, asserts `/skills/global` read-only); compiles/vets under GOOS=linux, live bwrap run deferred to Linux CI (host is darwin)
