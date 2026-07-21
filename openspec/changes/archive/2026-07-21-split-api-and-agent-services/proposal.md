## Why

The server runs as a single process that mixes long-running, resource-heavy agent execution (LLM calls, sub-agent orchestration, sandboxed code via bwrap) with lightweight CRUD (auth, sessions, message history, workspace files, skills). A surge in agent load or a crash in the agent path takes the whole process — and therefore the CRUD API — down with it. We want agent execution to run as an independent process so it can be scaled, restarted, or debugged without affecting the CRUD API, while keeping the data plane (MySQL, Redis, local `data/` directory) shared and unchanged.

This is **Phase 1 only**: same-machine deployment with a shared local filesystem. Cross-host horizontal scaling, object storage (MinIO), and removal of the filesystem warm tier are explicitly out of scope and deferred to a later change.

## What Changes

- The `serve` subcommand gains a **role selector** with three values: `api`, `agent`, and `all` (default `all`, which preserves today's single-process monolith behavior for dev, tests, and rollback).
- **Route ownership is partitioned by role:**
  - `api` role serves: auth/login, session CRUD, message history (GET), session title update, workspace file CRUD, skills list, and user seeding.
  - `agent` role serves: the streaming message endpoint (`POST /sessions/:id/messages`, including session lookup, history recovery, orchestrator run, persistence, and title generation) and the MCP tool list (`GET /api/v1/mcp/tools`).
  - `all` role serves both sets (current behavior).
- Each role runs its **own HTTP listener** on its own configured port (`server.port` for the API role; a new `server.agent_port` for the agent role), each with its own graceful-shutdown lifecycle.
- Both roles **share the same `-d` data root** and connect to the same MySQL / Redis / local filesystem. There is **no data-plane change**: the three-layer persistence, xizhi workspace tools, executor sandbox, and Landlock policy all behave exactly as today.
- The agent role owns the **full streaming-turn pipeline** (orchestrator, OpenAI client, tool registry, executor sandbox, TitleService); the API role depends on none of the agent layer.
- **Role-aware log filenames** so two processes do not contend on one lumberjack-managed file.
- The bootstrap (`serveRun`) is refactored into shared setup (config, logger, stores, Landlock) plus role builders `wireAPI` / `wireAgent`.
- **No request-routing logic is added.** How traffic reaches each role's port (reverse proxy, gateway, etc.) is handled externally and is explicitly out of scope for this change.

## Capabilities

### New Capabilities

- `service-roles`: Running the server in one of three deployment roles (`api`, `agent`, `all`) — role selection, role-scoped route registration, per-role HTTP listeners, shared runtime data root, role-aware logging, backward-compatible `all` role, and the fault-isolation property between the API and agent roles.

### Modified Capabilities

- `api-server`: The `serve` command-line interface gains the role selector; the single HTTP-server model becomes per-role (one listener per role, each with graceful shutdown); API routing is partitioned by role rather than registering every route unconditionally.

## Impact

- **Code:** `cmd/blowball/main.go` and `cmd/blowball/serve.go` (role selection + bootstrap split into shared setup and `wireAPI`/`wireAgent`); `internal/handler/router.go` and `RouteDeps` (role-scoped route registration); `internal/handler/session.go` (make the orchestrator dependency optional, or extract a streaming-only handler, so the API role does not depend on the agent layer); `internal/config` (the `server.agent_port` field and role validation); `internal/pkg/logger` (role-aware log filename); `Makefile` (run/build targets per role); `CLAUDE.md` (document the roles).
- **APIs:** Endpoint contracts are unchanged; only which process serves which endpoint changes. Frontend impact is none under a single-origin external routing setup.
- **Dependencies:** None added.
- **Out of scope:** cross-host / multi-replica scaling, MinIO or any object-storage backend, removal or relocation of the filesystem warm tier, workspace materialization, per-user concurrent-turn locking, and any external request routing between the two roles.
