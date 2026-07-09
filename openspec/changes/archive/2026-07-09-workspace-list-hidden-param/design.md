## Context

The workspace file listing endpoint (`GET /api/v1/workspace/files`) currently returns every entry in the requested directory, including hidden files and directories such as `.pip/`, `.git/`, or `.env`. The agent-facing xizhi discovery tools (`xizhi_list_files`, `xizhi_tree`, `xizhi_glob_files`) already support an `include_hidden` parameter that defaults to `false`. This change brings the REST API into alignment with those tools.

## Goals / Non-Goals

**Goals:**
- Add an `include_hidden` query parameter to `GET /api/v1/workspace/files`.
- Default `include_hidden` to `false` so hidden entries are excluded unless explicitly requested.
- Reuse the existing hidden-entry definition already used by xizhi tools (name begins with `.`).
- Update the OpenAPI document and handler tests.

**Non-Goals:**
- No changes to the frontend file tree in this change.
- No changes to agent tools, upload, download, delete, or content endpoints.
- No changes to the skills list endpoint (`GET /api/v1/skills`).
- No new user preference or persistence layer changes.

## Decisions

1. **Parameter name and type**: Use `include_hidden` as a boolean query parameter.
   - Rationale: Matches the existing xizhi tool parameter exactly, reducing cognitive overhead.
   - Alternative considered: `show_hidden`. Rejected to maintain consistency with xizhi tools.

2. **Default value**: `false` (exclude hidden entries).
   - Rationale: Matches xizhi behavior and keeps the default workspace view clean.
   - Trade-off: This is a breaking behavioral change for clients relying on hidden entries being present by default.

3. **Hidden-entry definition**: A name is hidden if it starts with `.` (the same rule in `internal/tool/xizhi/list.go`).
   - Rationale: Consistency with the agent tools. Simple, cross-platform, and covers the common clutter directories like `.pip` and `.git`.

4. **Implementation location**: Filter inside `WorkspaceHandler.List` after `os.ReadDir`.
   - Rationale: Minimal change, localized to the existing handler. No new service or store layer needed.

5. **OpenAPI representation**: Add `include_hidden` as an optional query parameter with `type: boolean` and `default: false`.
   - Rationale: Documents the new behavior for generated clients and frontend types.

## Risks / Trade-offs

- **[Risk] Breaking change for API consumers** → Existing clients that expect hidden files in the list will no longer see them.
  - **Mitigation**: Clearly document in the proposal/spec as breaking; clients can pass `include_hidden=true` to restore previous behavior.

- **[Risk] Inconsistent filtering if xizhi rules change later** → If the definition of "hidden" is extended in xizhi tools, the REST API may drift.
  - **Mitigation**: Consider exporting `isHiddenName` from `internal/tool/xizhi` so both paths share one function. For this minimal change, duplication is acceptable but noted as future cleanup.

- **[Risk] Query parameter parsing errors** → Gin parses boolean query parameters leniently; invalid values may be interpreted as `false`.
  - **Mitigation**: Use a helper that treats only explicit truthy strings (`true`, `1`) as `true`, and everything else as `false`, matching common expectations.

## Migration Plan

No deployment migration needed. After release, API consumers that require hidden entries must add `include_hidden=true` to their requests. Frontend can later add a user-facing toggle that sets this parameter.

## Open Questions

- Should the hidden-entry filter be shared with xizhi by exporting `isHiddenName`? (Deferred; can be a follow-up refactor.)
