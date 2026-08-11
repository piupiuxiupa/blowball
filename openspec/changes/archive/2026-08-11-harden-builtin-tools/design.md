## Context

The agent tool layer funnels every built-in tool call through a single execution path:

```
model tool_call
   │
   ▼  (per agent: Confucius / Chongzhi / Liang)
out, err := registry.Call(ctx, name, args)        ← internal/tool/registry.go
   │
   ▼
if err != nil → toolResult{content: err.Error(), isError: true}   (+ SSE agent_error)
else          → toolResult{content: marshalToolResult(out)}        ← internal/agent/confucius.go
   │
   ▼  toolResult.content becomes the role="tool" message fed back to the model
```

Three facts shape this change:

1. **`Registry.Call` is the only execution entry point** — all three agents and all tools (including MCP proxy tools) flow through it.
2. **`marshalToolResult` is the only result-rendering point** — defined once in `confucius.go`, shared by all three agents' dispatch sites.
3. **Success and failure are two different representation paths today** — success yields the tool's JSON object; failure yields a bare error string. There is no uniform status signal, and the failure path has no struct to tag.

Tool return shapes are mostly JSON objects, but two luban tools are not: `luban_list_skills` returns a **bare JSON array** and `luban_read_skill` returns a **bare string** (by design, `marshalToolResult` passes strings through un-encoded). Timeouts today are ad-hoc per family (`webfetch`, `executor bash`, per-user `mcp`) and entirely absent for the eight `xizhi_*` tools and `luban` list/read.

## Goals / Non-Goals

**Goals:**
- Give the model a machine-readable, uniform success/failure signal on every tool result.
- Give every built-in tool a configurable execution-time upper bound, enforced in one place.
- Stop `xizhi_grep` from silently searching the whole workspace when `path` is omitted.
- Let an agent enumerate a skill directory's files (flat + tree) without guessing paths.

**Non-Goals:**
- No change to the external HTTP/SSE contract (routes, event types, `done` usage shape).
- No replacement of the `agent_error` SSE event — the in-body `status` is an additional, model-facing channel.
- No per-tool timeout *default applied automatically* — unmapped tools keep current (no-timeout) behavior; operators opt tools in via the map.
- No tightening of `path` on `list_files` / `tree` / `glob_files` — only `xizhi_grep`.
- No new external API to manage the new skill tools — they are agent tools, registered like the existing luban tools.

## Decisions

### Decision 1: Both cross-cutting changes land at the shared chokepoint

The status envelope (变更 A) and the execution timeout (变更 B) are enforced at the two single shared points:

- **Timeout** → inside `Registry.Call`: `context.WithTimeout(ctx, r.timeouts[name])` before `spec.Execute`. Covers all three agents and all tools (including MCP proxies) with one edit.
- **Envelope** → replace `marshalToolResult(v any) string` with `renderToolResult(out any, err error) string`, called from the three dispatch sites (`confucius.go`, `chongzhi.go`, `liang.go`) with both `out` and `err`.

**Why:** these are the only places every tool result touches. Per-tool or per-agent enforcement would duplicate logic across 8+ tools / 3 agents and inevitably miss one (the failure path has no per-tool struct to begin with — see Decision 2). Central enforcement also means a timeout's `ctx.Err()` flows naturally into the envelope's failure branch: a timeout is just another `err`.

**Alternatives considered:**
- *Per-agent dispatch wrapping* — rejected: triples the edit sites and risks divergence; `Registry.Call` is already shared.
- *Per-tool `Timeout` field on `ToolSpec`* — rejected as the config source: would require threading config into every `RegisterAll` signature. The registry holding a name→duration map and looking it up in `Call` keeps registration signatures untouched.

### Decision 2: Wrap the result uniformly (envelope), do not inject

Every tool result becomes:

```
success → {"status":0,"result":<JSON-encoded tool return value>}
failure → {"status":1,"error":"<err.Error()>"}
```

**Why wrap over inject (adding `status` as a sibling field inside each tool's object):**
- The failure path returns `(nil, error)` — there is **no object** to inject into, so failure-side `status:1` must be synthesized centrally regardless. Once failure is centralized, centralizing success too is trivial and consistent.
- Two tools return **non-object** values: `luban_list_skills` (bare array) and `luban_read_skill` (bare string). A sibling key cannot be added to a JSON array or a bare string, so inject would special-case both. Wrap handles them uniformly: `{"status":0,"result":[...]}` / `{"status":0,"result":"..."}`.

**Alternatives considered:**
- *Inject into each tool's struct* — rejected: fails for the failure path and for non-object returns; also fragile (an author can forget to set the field).
- *Inject centrally via marshal→map→set key→re-marshal* — rejected: only works for object returns; still needs a wrap fallback for the two non-object tools, yielding two shapes. The user explicitly chose "all tools one shape", which only wrap satisfies.

**`[]byte` edge case:** `marshalToolResult` today treats `[]byte` as raw text (`string(x)`), not base64. `renderToolResult` MUST preserve this — normalize `[]byte`/`string` returns to a Go string before JSON-encoding the `result` field, so a `[]byte`-returning tool does not get base64-encoded inside the envelope.

### Decision 3: Timeout config is a flat name→duration map

```yaml
tools:
  timeouts:
    xizhi_grep: 30s
    xizhi_tree: 15s
    webfetch: 30s
    bash: 120s
```

New field `ToolsConfig.Timeouts map[string]time.Duration`. The registry receives it once at wiring time (`SetTimeouts`); `Call` looks up `r.timeouts[name]`.

**Why:** keyed by the same tool-name string agents already use in their `tools:` lists, so it is self-describing and lives in one place. It composes cleanly with the existing family-specific timeouts: for remote tools (`webfetch`/`mcp`/`bash`) the native timeout remains the tighter inner bound and the new timeout is a looser outer backstop; for the timeout-less local tools (`xizhi_*`, `luban` list/read) it is the first and only bound.

**Semantics:** an unmapped entry or zero duration means **no timeout** (byte-for-byte current behavior). This is deliberate backward compatibility — the change is opt-in per tool, not a global default.

**Alternatives considered:**
- *Add `Timeout` to each `XizhiToolConfig` / family config* — rejected: verbose, and `webfetch`/`bash` already have a `Timeout` field whose semantics would collide with a second outer timeout on the same name.
- *Global `tools.default_timeout` + overrides* — rejected: the user chose a pure name-map; a hidden global default would surprise operators who expect unmapped = unchanged.

### Decision 4: `xizhi_grep` requires `path`; `"."` still means root

Drop the `normalizePath("")` → `"."` fallback inside `GrepFiles`; an empty/whitespace `path` returns `xizhi_grep: path is required`. Add `path` to the schema `required` array and update the description. `"."` continues to validate normally, so it still means "search the root" — this cleanly separates "the model forgot the path" (error) from "the model explicitly wants the whole workspace" (`.`).

**Why grep only:** it is the one discovery tool that reads file *contents* (expensive: full scan, ~200-match cap). `list_files`/`tree`/`glob_files` are cheap metadata reads where defaulting to root is convenient; tightening them is out of scope and would needlessly break existing prompts.

### Decision 5: New skill tools join the luban family

`luban_list_skill_files` and `luban_tree_skill` are registered by the existing `luban.RegisterAll` path and activated by the existing `needsLubanTools` gate. They mirror `xizhi_list_files` / `xizhi_tree` shapes. Name→directory resolution reuses `skill.Loader` (so user-overrides-global precedence is identical to `luban_read_skill`); sub-`path` confinement reuses the `luban_read_skill` `ReadPath` security checks (absolute/`..`/symlink-escape rejected, binary not applicable to listings). `.git` (present on git-cloned skills) is hidden by the default `include_hidden: false`, mirroring xizhi.

**Why luban, not xizhi:** skills live under `data/{userID}/skills` and `{data-dir}/skills`, outside the `data/{userID}/workspace` subtree that xizhi is scoped to (and xizhi intentionally cannot reach). A skill-scoped tool is the only enumeration path; luban already owns the skill namespace and its confinement primitives.

## Risks / Trade-offs

- **[Envelope changes persisted message content]** → Tool-result bodies stored in MySQL/FS/Redis now carry the envelope. `message_reconstruct` replays verbatim and does not parse content, so reconstruction is unaffected; but a frontend that pretty-prints or parses `tool_result.content` as the raw tool object will see `{"status":0,"result":{...}}` instead. Mitigation: coordinate the frontend change; the inner `result` shape is unchanged, so extraction is a one-level dereference.
- **[Envelope is a model-facing breaking change]** → Models that learned to expect bare objects/strings now see wrapped ones. Mitigation: rewrite the two affected luban descriptions (`luban_list_skills`, `luban_read_skill`) so advertised shape matches reality; all other tool descriptions only need the envelope noted generically.
- **[Timeout may interrupt long-but-valid operations]** → e.g. a legitimately large `xizhi_tree` at depth 10. Mitigation: defaults are generous and operator-tunable; unmapped = unbounded, so nothing changes unless an operator opts a tool in.
- **[Double timeout on remote tools]** → A central timeout plus a native inner timeout could confuse debugging ("which limit fired?"). Mitigation: document that the central timeout must be set *looser* than the native one; the tighter (native) bound fires first in practice.
- **[Empty-path grep is a prompt contract change]** → Existing prompts/skills that call `xizhi_grep` without `path` now error. Mitigation: the error message tells the model `path is required`; `"."` is the explicit-root escape hatch.

## Migration Plan

1. Ship the envelope + timeout together (they share the chokepoint); the timeout's error path is exercised through the envelope from day one.
2. Add `tools.timeouts` to `config.example.yaml` as commented examples — no existing deployment changes behavior unless an operator adds entries.
3. Update the two luban tool descriptions in the same change so description/reality never diverge.
4. No DB migration: stored message content changes shape but no schema column or type changes.
5. **Rollback:** revert the change; tool-result bodies revert to bare objects/strings. Stored messages written under the envelope remain valid JSON and replay fine (they are not parsed), so a rollback mid-stream does not corrupt history — only newly written turns revert to the old shape.

## Open Questions

None blocking. (The recommended `tools.timeouts` default values in `config.example.yaml` are illustrative and can be tuned during implementation.)
