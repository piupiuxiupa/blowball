## 1. Skill Discovery Refactoring

- [x] 1.1 Modify `internal/tool/skill/skill.go` `discover` to recursively scan subdirectories for `SKILL.md`
- [x] 1.2 Add `Loader.ListGlobal(userID string)` or equivalent method that returns only global skills for system prompt injection
- [x] 1.3 Ensure user skill override semantics still work with recursive discovery
- [x] 1.4 Add/update unit tests for recursive skill discovery and global-only listing

## 2. Luban Tool Package

- [x] 2.1 Create `internal/tool/luban/validate.go` with skill name/path validation
- [x] 2.2 Create `internal/tool/luban/list.go` implementing `luban_list_skills`
- [x] 2.3 Create `internal/tool/luban/read.go` implementing `luban_read_skill`
- [x] 2.4 Create `internal/tool/luban/install.go` implementing `luban_install_skill` with git clone and single-file download
- [x] 2.5 Create `internal/tool/luban/register.go` to register all three tools with the registry
- [x] 2.6 Add unit tests for each luban tool (list, read, install, validation)

## 3. System Prompt and Orchestrator Updates

- [x] 3.1 Update `internal/prompt/render.go` Skills section to reference luban tools and forbid xizhi access to skills
- [x] 3.2 Update `internal/agent/orchestrator.go` `collectSkills` to inject only global skills
- [x] 3.3 Update `internal/agent/orchestrator.go` `buildAgentRegistry` to copy luban tools from baseRegistry and add `isLubanTool` helper
- [x] 3.4 Update `internal/agent/orchestrator.go` to stop auto-adding `read_skill` when skills are configured
- [x] 3.5 Add/update tests for system prompt rendering and orchestrator registry building

## 4. Server Wiring and Configuration

- [x] 4.1 Update `cmd/server/main.go` to register luban tools at startup
- [x] 4.2 Update `cmd/server/main.go` skill loader wiring to support global-only queries
- [x] 4.3 Replace `needsReadSkill` logic with `needsLubanTools` based on agent tool lists
- [x] 4.4 Update `config.yaml` to replace `read_skill` with luban tools in agent tool lists
- [x] 4.5 Add example global skills to `agents.*.skills` in `config.yaml` if desired

## 5. Validation and Cleanup

- [x] 5.1 Run `make test` and fix any failures
- [x] 5.2 Run integration tests for agent/orchestrator with luban tools
- [x] 5.3 Verify `luban_install_skill` can install `superpowers` repo and `luban_list_skills` discovers nested skills
- [x] 5.4 Verify system prompt no longer tells model to use `read_skill`
- [x] 5.5 Decide whether to fully remove `read_skill` registration or keep it for backward compatibility
