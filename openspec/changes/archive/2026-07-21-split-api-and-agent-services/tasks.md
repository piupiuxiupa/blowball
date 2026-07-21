## 1. Config & role flag foundation

- [x] 1.1 Add `server.agent_port` field (default `8081`) to the server config in `internal/config/config.go` and `config.example.yaml`.
- [x] 1.2 Add a `--role` local flag (`all` | `api` | `agent`, default `all`) to the `serve` cobra subcommand in `cmd/blowball/main.go` / `cmd/blowball/serve.go`; reject invalid values with a non-zero exit before any setup.
- [x] 1.3 Relax agent-only config validation for the `api` role: a missing/empty `openai` key logs a warning instead of failing startup when `--role api`.

## 2. Role-aware logging

- [x] 2.1 Thread the selected role into `logger.Init` and derive the lumberjack filename: `blowball.log` (`all`), `blowball-api.log` (`api`), `blowball-agent.log` (`agent`), all under `{data-dir}/logs/`.
- [x] 2.2 Add/adjust unit tests in the logger package covering the three filenames.

## 3. Bootstrap split

- [x] 3.1 Extract the shared setup (flags → config → runtime dir derivation → `MkdirAll(logDir)` → `logger.Init` → MySQL/Redis/`fsStore` → `MkdirAll(skills/tools)` → `ApplyLandlock`) into a shared function used by every role.
- [x] 3.2 Implement `wireAPI(...)` that builds the CRUD services/handlers (auth, session CRUD, message-history read, workspace, skills) and returns a `*gin.Engine` registering only API routes (no orchestrator, no MCP client, no tool registry).
- [x] 3.3 Implement `wireAgent(...)` that builds the tool registry + MCP manager + orchestrator + OpenAI client + TitleService + streaming handler and returns a `*gin.Engine` registering the streaming endpoint and `/mcp/tools` (both behind JWT auth middleware).
- [x] 3.4 Dispatch in `serveRun`: `all` → merge `wireAPI`+`wireAgent` route groups onto one engine on `server.port`; `api` → `wireAPI` on `server.port`; `agent` → `wireAgent` on `server.agent_port`. Preserve the current middleware order (Recovery → Trace → CORS → Auth) for each engine.
- [x] 3.5 Emit a startup log line stating `role`, listen port, and the registered route groups.

## 4. Handler refactor (decouple API role from the agent layer)

- [x] 4.1 Extract `SendMessage` and its private helpers (`persistEvents`, etc.) from `internal/handler/session.go` into a new `MessageStreamHandler` (holds `sessSvc`, `msgSvc`, `titleSvc`, `orch`, `dataDir`, hub/SSE helpers) — behavior unchanged.
- [x] 4.2 Remove the `orch` dependency from `SessionHandler` and `NewSessionHandler`; `SessionHandler` now owns only CRUD methods (`ListSessions`, `CreateSession`, `GetSessionMessages`, `DeleteSession`, `UpdateTitle`).
- [x] 4.3 Update `internal/handler/ports.go` / router wiring so the streaming route is registered by `MessageStreamHandler` and the CRUD routes by `SessionHandler`.
- [x] 4.4 Update `RouteDeps` so route registration can be partitioned: split into API-route deps and agent-route deps (or add a role parameter to `RegisterRoutes`), ensuring `api` registers CRUD only and `agent` registers streaming + `/mcp/tools` only.
- [x] 4.5 Move existing `session_test.go` streaming tests to cover `MessageStreamHandler`; ensure CRUD tests still pass against the slimmed `SessionHandler`.

## 5. Route partitioning & per-role listeners

- [x] 5.1 Verify the API-role engine does NOT register `POST /api/v1/sessions/:session_id/messages` or `GET /api/v1/mcp/tools` (requests return 404).
- [x] 5.2 Verify the agent-role engine does NOT register session/workspace/skills CRUD routes (requests return 404).
- [x] 5.3 Verify the `all`-role engine registers the complete current route set on a single `server.port` listener (back-compat with existing integration tests).
- [x] 5.4 Ensure each role's HTTP server has its own graceful-shutdown lifecycle (SIGINT/SIGTERM → drain → close stores).

## 6. Tests

- [x] 6.1 Add unit/router tests asserting the exact route set registered under each role matches the `service-roles` spec partition.
- [x] 6.2 Add a test that the `api` role construction does not instantiate the orchestrator / OpenAI client / tool registry (fault-isolation / no-agent-dependency requirement).
- [x] 6.3 Add a test that a streaming turn run under the `agent` role persists results readable via the shared store (agent owns full pipeline).
- [x] 6.4 Keep the existing `test/integration/` harness running under the default role (`all`) unchanged; add a focused integration case exercising `--role api` and `--role agent` route ownership if feasible.

## 7. Docs & build

- [x] 7.1 Update `CLAUDE.md`: document the three roles, per-role ports, what each role wires, shared data plane, and that external request routing is operator-managed and out of scope.
- [x] 7.2 Add `make`/usage examples for `serve --role api` and `serve --role agent` (and note `serve` == `--role all` rollback).
- [x] 7.3 Run `make lint` and `make test`; ensure no regressions.
