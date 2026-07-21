## Context

Today `bin/blowball serve` is one process that registers every route (auth, sessions, message streaming, workspace, skills, MCP) on a single Gin listener, and wires the full stack — MySQL/Redis/FS stores, the orchestrator, the OpenAI client, the tool registry, the bwrap executor sandbox, and Landlock — into that one process. The streaming endpoint (`POST /api/v1/sessions/:session_id/messages`) is the only place where the lightweight CRUD path and the heavy agent-execution path meet inside one handler: `SessionHandler.SendMessage` does session lookup + history recovery, then runs the orchestrator while writing SSE, then persists the turn and fires title generation.

We want agent execution to run as an independent process so a surge in agent load or an agent-path crash cannot take down the CRUD API. Phase 1 constrains this to **same-machine deployment with a shared local filesystem** and **no data-plane change**; external request routing between the two roles is handled outside this change.

The seam already exists: `internal/handler/ports.go` defines `OrchestratorRunner`, the only contract between the handler layer and the agent layer, and `SessionDeps` bundles only store interfaces. So the split is mostly a wiring/routing problem, not a domain-logic problem.

## Goals / Non-Goals

**Goals:**
- Run the server in one of three roles — `api`, `agent`, `all` (default) — from the same binary.
- Partition route registration and the HTTP listener by role; `all` preserves current single-process behavior for dev, tests, and rollback.
- Give the `agent` role full ownership of the streaming-turn pipeline (lookup → recover → orchestrate → SSE → persist → title) and all heavy infra (LLM client, tool registry, executor sandbox); give the `api` role zero dependency on the agent layer.
- Keep the data plane unchanged: same `-d` root, same MySQL/Redis/FS, same three-layer persistence, same xizhi/executor/Landlock behavior.
- Achieve fault isolation: either role's process crashing does not stop the other role's own endpoints.

**Non-Goals:**
- Cross-host / multi-replica horizontal scaling (requires shared/networked storage — deferred; Phase 1 is same-machine only).
- MinIO or any object-storage backend.
- Removing or relocating the filesystem warm tier; workspace materialization; per-user concurrent-turn locking.
- Any external request routing (reverse proxy, gateway, ingress) — explicitly the operator's responsibility and out of scope.
- Splitting into two separate binaries/images; Phase 1 is one binary selected by `--role`.
- Changing any endpoint contract or the frontend.

## Decisions

### D1. Role selection: a `--role` flag on `serve`, not subcommands or a second binary
`serve` gains a local flag `--role` (`all` | `api` | `agent`, default `all`).

**Why over alternatives:**
- *`serve api` / `serve agent` subcommands*: would turn `serve` from a leaf command into a parent, which is awkward in cobra alongside its existing `RunE` and the persistent `-f`/`-d` flags. A flag keeps the command tree flat.
- *Two separate binaries (`blowball-api` / `blowball-agent`)*: cleaner long-term, but doubles build targets, images, and config files for Phase 1 with no benefit over a flag. The flag still lets the operator run two processes; we can split binaries later if warranted.

The `all` default is deliberate: it is the rollback path and the mode the existing integration tests and local dev use unchanged.

### D2. `all` runs a single listener on `server.port`; `api` on `server.port`; `agent` on `server.agent_port`
- `all`: one Gin engine, all routes, listens on `server.port`. Identical to today.
- `api`: one Gin engine, CRUD routes only, listens on `server.port`.
- `agent`: one Gin engine, streaming + MCP-tools routes only, listens on `server.agent_port` (new config field, default `8081`).

**Why `all` keeps a single listener** rather than also opening `server.agent_port`: so any existing caller (tests, dev Vite proxy, monitoring hitting `:8080`) keeps working without change. `all` is the backward-compatible shape; the two-listener topology is the property of the split roles only.

### D3. Bootstrap refactor: shared setup + role builders
`serveRun` is split into:
- **Shared setup** (runs for every role): resolve flags → load config → derive `dataDir`/`logDir`/`skillsDir`/`toolsDir` → `MkdirAll(logDir)` → `logger.Init` (role-aware filename, see D6) → connect MySQL/Redis → construct `fsStore` → `MkdirAll(skills/tools)` → `ApplyLandlock`.
- **`wireAPI(cfg, deps, ...)`**: builds the CRUD services/handlers (auth, session CRUD, message-history read, workspace, skills) and returns a `*gin.Engine` registering only the API routes.
- **`wireAgent(cfg, deps, ...)`**: builds the tool registry, MCP manager, orchestrator, OpenAI client, TitleService, the streaming handler, and returns a `*gin.Engine` registering the streaming + MCP-tools routes (both behind JWT auth middleware).

The dispatch is:
```
role == "all"  → shared setup; engine = merge(wireAPI, wireAgent); listen on server.port
role == "api"  → shared setup; engine = wireAPI;                 listen on server.port
role == "agent"→ shared setup; engine = wireAgent;               listen on server.agent_port
```
For `all`, the two builders contribute their route groups to the same engine (equivalent to today's `RegisterRoutes`), preserving the exact current route set and middleware order.

### D4. Extract a streaming-only handler so the API role never depends on the orchestrator
Today `SessionHandler` owns both CRUD methods and `SendMessage`, and `NewSessionHandler` requires an `OrchestratorRunner`. To keep the `api` role free of the agent layer, extract the streaming concern into a new `MessageStreamHandler` (name tentative) that holds `sessSvc`, `msgSvc`, `titleSvc`, `orch`, `dataDir`, and the hub/SSE helpers — i.e., it absorbs `SendMessage` and its private helpers (`persistEvents`, etc.) verbatim.

- `SessionHandler` is left with only CRUD methods (`ListSessions`, `CreateSession`, `GetSessionMessages`, `DeleteSession`, `UpdateTitle`) and no longer takes an orchestrator.
- `wireAPI` constructs `SessionHandler` (no orchestrator reachable).
- `wireAgent` constructs `MessageStreamHandler` (with orchestrator) and still needs `sessSvc`/`msgSvc`/`titleSvc` for the streaming endpoint's own lookup/recover/persist/title steps.

**Why over the alternative** (make `orch` an optional/nil field on `SessionHandler`): a nil-orch `SessionHandler` in the API role is a footgun — it keeps a dead field and lets a future caller accidentally register `SendMessage` where no orchestrator exists. Extraction makes the dependency boundary explicit and compile-time honest.

### D5. `/mcp/tools` moves to the agent role
The MCP manager + tool registry are built once at startup and are used by the orchestrator; they live naturally in the agent role. `wireAPI` does not connect MCP clients or build the registry, so the API role serves no `/mcp/tools`. In the `all` role the endpoint is registered as today. (Endpoint contract unchanged; only ownership moves.)

### D6. Role-aware log filename
`logger.Init` receives the role and derives the lumberjack filename: `blowball.log` for `all`, `blowball-api.log` for `api`, `blowball-agent.log` for `agent`, all under `{data-dir}/logs/`. This prevents two lumberjack instances from contending on one file (rotation races) when both roles run on the same host.

### D7. Stores and Landlock are shared/unchanged
Both roles construct the same `SessionDeps{MySQL, Redis, FS}` and connect the same MySQL/Redis; both call `ApplyLandlock(dataDir, logDir, skillsDir, [toolsDir])` with the same policy as today (read-write for data/logs/skills, read-only for tools). No per-role data isolation. The three-layer write path, `xizhi` workspace tools, the bwrap executor sandbox, and skill loading behave exactly as in the monolith — they just happen to live in the agent-role process now.

### D8. Config additions
- `server.agent_port` (default `8081`): the agent-role listener port.
- No other config changes. The shared `config.yaml` is read by both roles; the API role simply ignores OpenAI/tool/agent configs it does not use (a startup `debug` log notes which sections are unused for the role). Validation that would today reject a missing OpenAI key is relaxed to a warning for the `api` role, since it never calls the LLM.

## Risks / Trade-offs

- **Doubled store connections:** two processes each open their own MySQL/Redis pools. → Mitigation: keep pool sizes modest; acceptable on one host. Document in CLAUDE.md.
- **Mis-roled deployment:** an operator running two `api` roles, or pointing the streaming route at the API port, sees 404s/503s at the (external) routing layer. → Mitigation: the startup log line clearly states `role`, `port`, and the registered route groups; external routing correctness is the operator's concern and is documented as out of scope.
- **`all` back-compat drift:** any behavior difference between `all` and running `api`+`agent` together would be a silent bug. → Mitigation: integration tests keep using the default role (effectively `all`); add focused tests asserting the route sets registered under `api` and `agent` separately match the spec partition.
- **Heavy agent binary:** the agent role still imports the full service layer (as the monolith did) — no new coupling, but the agent process remains the "fat" one. This is expected and fine for Phase 1.
- **No cross-role trace correlation:** each HTTP request is served wholly by one role, so a trace never spans both processes. (The streaming endpoint's internal steps all live in the agent role.) → No action needed; noted to avoid the false expectation of a cross-process trace.
- **Concurrent same-user writes:** the only writers to a session's FS/Redis/MySQL rows are the agent role (turn persistence) and the API role (create/delete session). Create precedes any turn; delete-during-turn is a pre-existing race already possible in the monolith. → No new hazard in Phase 1; the per-user concurrent-turn contract is a Phase 2 concern (explicitly out of scope).

## Migration Plan

1. **Build** the unified binary (unchanged `make build`).
2. **Run both roles** on the same host:
   - `./bin/blowball serve --role api   -d <root> -f <config>`
   - `./bin/blowball serve --role agent -d <root> -f <config>`
3. **External routing** (operator): direct `POST /api/v1/sessions/:session_id/messages` and `GET /api/v1/mcp/tools` to the agent port; everything else to the API port.
4. **Smoke test:** CRUD endpoints via the API port; one streaming turn via the agent port; verify history written by the agent is readable via the API port.
5. **Rollback:** stop both role processes and run `./bin/blowball serve` (default `all`) — single process, full behavior, on `server.port`. No data migration in either direction (data plane unchanged).

## Open Questions

- Confirm `server.agent_port` default of `8081` is acceptable (or whether to require it explicitly when `--role agent` is used).
- Whether a missing OpenAI key under `--role api` should be a warning (preferred) or silently ignored — leaning warning.
- Whether to emit a startup warning if two roles appear to share a port by misconfiguration (best-effort, not required for Phase 1).
