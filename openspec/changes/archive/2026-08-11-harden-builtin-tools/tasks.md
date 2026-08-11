## 1. Tool-result status envelope (变更 A)

- [x] 1.1 In `internal/agent/confucius.go`, replace `marshalToolResult(v any) string` with `renderToolResult(out any, err error) string` that returns `{"status":1,"error":<json string of err.Error()>}` when `err != nil`, else `{"status":0,"result":<json-encoded out>}`. Marshal via a small struct `{Status int; Result json.RawMessage; Error string}` so field order/omission is exact (omit `result` on failure, omit `error` on success).
- [x] 1.2 In `renderToolResult`, normalize `[]byte` and `string` returns to a Go string **before** JSON-encoding into `result`, so byte-slice returns are NOT base64-encoded (preserve prior `marshalToolResult` semantics). `nil` out → `{"status":0,"result":null}`.
- [x] 1.3 Update the three dispatch call sites to pass both values and set `isError` from the error: `internal/agent/confucius.go:407`, `internal/agent/chongzhi.go:221`, `internal/agent/liang.go:214` → `return toolResult{content: renderToolResult(out, err), isError: err != nil}`.
- [x] 1.4 In `internal/tool/luban/register.go`, rewrite the `luban_list_skills` and `luban_read_skill` `Description` strings so they declare the result is delivered inside the `{"status","result"}` envelope (no longer "return an array" / "bare string ... not a JSON object").
- [x] 1.5 Add a unit test for `renderToolResult` covering: object success, array success, bare-string success, `[]byte` success (assert NOT base64), nil success (`result:null`), and error (`status:1` + `error` + no `result`).

## 2. Per-tool execution timeout (变更 B)

- [x] 2.1 In `internal/config/config.go`, add `Timeouts map[string]time.Duration` to `ToolsConfig` (struct at line 741) with yaml tag `timeouts`.
- [x] 2.2 In `internal/tool/registry.go`, add a `timeouts map[string]time.Duration` field to `Registry` and a `SetTimeouts(m map[string]time.Duration)` method.
- [x] 2.3 In `Registry.Call` (`registry.go:117`), before `spec.Execute`, look up `r.timeouts[name]`; if present and `> 0`, derive `ctx, cancel := context.WithTimeout(ctx, d)` (defer cancel) and pass the child context to `Execute`. Absent/zero → unchanged.
- [x] 2.4 In `cmd/blowball/serve.go`, after `reg := tool.NewRegistry()` (line 436), call `reg.SetTimeouts(cfg.Tools.Timeouts)` so all subsequently registered tools inherit the map. (Also applied at the per-request execution registry in `internal/agent/orchestrator.go`, which is the actual tool-execution path during a turn.)
- [x] 2.5 Add a commented example block to `config.example.yaml` under `tools:`: `# timeouts: { xizhi_grep: 30s, xizhi_tree: 15s, xizhi_list_files: 10s, xizhi_glob_files: 15s, webfetch: 30s, bash: 120s }`.
- [x] 2.6 Add a registry unit test: a tool configured with a tiny timeout that sleeps longer surfaces a deadline-exceeded error through `Call`; an unmapped tool is unbounded (no cancellation).

## 3. xizhi_grep required path (变更 C)

- [x] 3.1 In `internal/tool/xizhi/grep.go` `GrepFiles` (line 68), remove the `relPath = normalizePath(relPath)` fallback; instead, after decoding args, reject empty/whitespace `path` with `return nil, fmt.Errorf("xizhi_grep: path is required")` before `validatePath`. Keep `"."` valid (it still passes `validatePath`).
- [x] 3.2 In `internal/tool/xizhi/register.go`, change `schemaGrep` `required` from `["pattern"]` to `["path","pattern"]` (line ~164) and update the `path` property description (line ~136) to mark it required and remove "Defaults to the workspace root".
- [x] 3.3 Update the `xizhi_grep` tool `Description` string (register.go ~339) to state `path` is required and `"."` means root.
- [x] 3.4 Add/update grep tests: empty `path` → error; omitted `path` (schema) → required; `"."` → searches root. Confirm `xizhi_list_files`/`xizhi_tree`/`xizhi_glob_files` still accept empty path (unchanged).

## 4. Skill directory tools (变更 D)

- [x] 4.1 Add exported tool-name constants `ToolListSkillFiles = "luban_list_skill_files"` and `ToolTreeSkill = "luban_tree_skill"` in `internal/tool/luban/` (alongside the existing `ToolListSkills` etc.).
- [x] 4.2 Implement `ListSkillFiles(loader, name, subPath, userID, includeHidden)` and `TreeSkill(loader, name, subPath, userID, depth, includeHidden)` in a new `internal/tool/luban/skill_files.go`. Resolve the skill directory from `name` via `skill.Loader` (user overrides global, identical precedence to `luban_read_skill`); apply the same sub-`path` confinement as `skill.ReadPath` (reject absolute / `..` escape / symlink escape). Return shapes `{path, entries:[{name,type,size}]}` and `{path, depth, tree:[{name,type,size?,children?}]}`, mirroring `xizhi_list_files` / `xizhi_tree`. Reuse the skill package's existing frontmatter-aware loader only for directory resolution; listing/tree is plain `os.ReadDir`/walk with the same hidden-name and depth semantics as xizhi. (Confinement reuses the skill package's newly-exported `ValidateSubPath`; name→directory resolution reuses the newly-added `Loader.SkillDir`.)
- [x] 4.3 Register both tools in `internal/tool/luban/register.go` `RegisterAll`: add `registerListSkillFiles` and `registerTreeSkill` with JSON-Schema parameters (`name` required; optional `path`, `include_hidden`; `depth` for tree) and descriptions that declare the `{status, result}` envelope return and the luban-not-xizhi pointer.
- [x] 4.4 In `cmd/blowball/serve.go` `needsLubanTools` (line 578), add `luban.ToolListSkillFiles` and `luban.ToolTreeSkill` to the activating tool-name slice so an agent listing either activates the luban family.
- [x] 4.5 Add luban tests: list/tree of a skill root; sub-directory; user-overrides-global; `.git` hidden by default; path-traversal rejected; unknown skill → error; depth clamp to 10.

## 5. Integration & validation

- [x] 5.1 Run `make test` (unit) and `go test ./test/integration/...` (real orchestrator + handlers) to confirm the envelope appears in streamed tool results and no dispatch path regresses. (All touched packages + integration green. One pre-existing flaky teardown race in `internal/tool/mcp` `TestListTools_ReturnsLiveToolList` — intermittent ~1/8, in a package this change does not touch — was confirmed via stash/restore against clean master.)
- [x] 5.2 Run `make lint`. (`go vet ./...` exits 0; this change's files are gofmt-clean. The repo does not enforce gofmt and has pre-existing violations in untouched files, left as-is.)
- [x] 5.3 Update `CLAUDE.md` tool-family notes: mention the `{"status","result"}` envelope in the tool/SSE section, `tools.timeouts` in the config section, `xizhi_grep` path-required, and the two new luban tools in the skills/tools section.
