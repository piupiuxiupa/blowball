## 1. Prompt Rendering

- [x] 1.1 Extend `internal/prompt.RenderInput` with `GlobalSkillsDir` and `UserSkillsDir` fields
- [x] 1.2 Update `renderEnvironment` to emit both skill directory paths as sandbox-resolvable paths (`/skills/global`, `/skills/user`)
- [x] 1.3 Update the `## Skills` section to tell agents they may use `bash`/`python` tools to read/execute skill files and that skill directories are read-only
- [x] 1.4 Update `internal/prompt` unit tests to assert both skill directories appear in the environment section

## 2. Orchestrator Wiring

- [x] 2.1 Update `internal/agent/orchestrator.go` `renderSystemPrompt` signature and call site to pass global and user skills directories
- [x] 2.2 Update `buildAgentRegistry` to receive both skill directory paths from `Build`
- [x] 2.3 Ensure the global skills directory path comes from the configured loader (`skills/`) and the user skills directory from `data/{userID}/skills`

## 3. Executor Tool Sandbox

- [x] 3.1 Extend `internal/tool/executor.Tools` with `globalSkillsDir` and `userSkillsDir` fields
- [x] 3.2 Update `executor.NewTools` to accept global and user skills directory paths
- [x] 3.3 Update `internal/tool/executor/bwrap.go` `buildBwrapArgs` to accept skill directories and add `--ro-bind {globalSkillsDir} /skills/global` and `--ro-bind {userSkillsDir} /skills/user`
- [x] 3.4 Update `internal/tool/executor/register.go` to pass skill directories into `tools.run`
- [x] 3.5 Add/update executor tool tests to assert skill directories are mounted read-only inside the sandbox

## 4. Server Wiring

- [x] 4.1 Update `cmd/server/main.go` to pass the global skills directory (`skills/`) and the per-user skills directory closure (`fsStore.UserSkills`) into `executor.NewTools`
- [x] 4.2 Verify executor tools are only registered on Linux with bwrap, and that skill directory mounting does not affect non-Linux startup

## 5. Skill Documentation

- [x] 5.1 Update `skills/ifind-finance-data/SKILL.md` to reference sandbox paths (`/skills/global/ifind-finance-data/call-node.js`, `/skills/global/ifind-finance-data/mcp_config.json`, `/skills/global/ifind-finance-data/references/...`)
- [x] 5.2 Verify the skill instructions still work after path changes

## 6. Integration Verification

- [x] 6.1 Run `make test` and fix any regressions in prompt rendering, executor tools, or orchestrator tests
- [x] 6.2 Run integration tests (`go test ./test/integration/...`) to ensure the full request flow still works
- [x] 6.3 Manually verify on Linux that a sandboxed `bash` command can list `/skills/global/ifind-finance-data/`
