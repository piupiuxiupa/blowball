## Why

The agent tool layer has four independent gaps that each erode reliability or observability of a turn: tool failures reach the model as free-text error strings (no machine-readable success/failure signal), local tools (`xizhi_*`, `luban` list/read) have no execution timeout and can hang a turn on pathological input, `xizhi_grep` silently searches the entire workspace when the model omits `path`, and there is no way for an agent to enumerate the files inside a skill directory (it must guess sub-paths before it can read them). These are cheap to fix and share a single execution chokepoint, so they are bundled into one change.

## What Changes

- **Uniform tool-result status envelope.** Every built-in tool's result is wrapped as `{"status":0,"result":<tool return value>}` on success and `{"status":1,"error":"<message>"}` on failure, rendered centrally. The model gets a consistent success/failure signal instead of parsing free-text errors. **BREAKING** for the tool-result message body shape (persisted in messages and surfaced to the model/frontend); the external SSE/HTTP API is unchanged.
- **Per-tool execution timeout config.** A new `tools.timeouts` map (tool name → duration) is enforced centrally at the registry execution entry point. Tools not listed keep current behavior (no timeout). Remote tools (`webfetch`, `mcp`, `bash`) keep their existing native timeouts as tighter inner bounds; the new timeout is an outer backstop that, for the first time, also bounds the timeout-less local tools.
- **`xizhi_grep` requires `path`.** An empty/blank `path` now errors instead of silently searching the workspace root. **BREAKING** for the tool's input contract (a call omitting `path` that previously succeeded now fails); `"."` still means the root. `list_files` / `tree` / `glob_files` are unchanged.
- **Two new skill-directory tools.** `luban_list_skill_files` and `luban_tree_skill` enumerate a skill's directory tree (flat list and nested tree), mirroring `xizhi_list_files` / `xizhi_tree`, scoped to a skill resolved by name (user overrides global) with the same path-confinement security as `luban_read_skill`.

## Capabilities

### New Capabilities
- `tool-result-envelope`: the uniform `{"status","result"|"error"}` envelope applied to every built-in tool result, and the central rendering point that produces it.
- `tool-execution-timeout`: the operator-configurable per-tool execution timeout map and its central enforcement at the registry dispatch entry point.

### Modified Capabilities
- `xizhi-grep-files`: `path` becomes a required argument; an empty/blank path is rejected.
- `luban-skill-tools`: adds the `luban_list_skill_files` and `luban_tree_skill` tools for enumerating a skill directory's contents.

## Impact

- **Code**: `internal/tool/registry.go` (timeout enforcement + timeouts map on `Registry`); `internal/agent/confucius.go` (`marshalToolResult` → `renderToolResult(out, err)`) and the parallel dispatch sites in `chongzhi.go` / `liang.go`; `internal/tool/xizhi/grep.go` + `internal/tool/xizhi/register.go` (grep `path` required); `internal/tool/luban/register.go` (two new tools + two description rewrites for the envelope); `internal/tool/skill/skill.go` (reuse `ReadPath`-style confinement); `internal/config/config.go` (`ToolsConfig.Timeouts`); `cmd/blowball/serve.go` (registration unchanged, luban activation via existing `needsLubanTools`); `config.example.yaml` (example timeouts).
- **Model-facing contracts**: every tool's returned JSON shape changes (envelope); `luban_list_skills` / `luban_read_skill` tool descriptions are rewritten to match (they previously advertised a bare array / bare string); `xizhi_grep` description and JSON Schema mark `path` required.
- **Persistence**: tool-result message bodies stored in MySQL/FS/Redis carry the new envelope; `message_reconstruct` replays verbatim (no parsing), so no reconstruction logic changes — only the stored content is richer.
- **No external HTTP/SSE contract change**: routes, event types, and the `done` usage shape are untouched. The `agent_error` SSE event (frontend signal) coexists with the new in-body `status` and is not replaced.
