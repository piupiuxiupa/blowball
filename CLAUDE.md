# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Blowball is a Go backend for a multi-agent chat workspace. It exposes a JWT-secured HTTP API (Gin), persists sessions/messages in MySQL with Redis caching and filesystem warm storage, and orchestrates OpenAI-backed agents. (The React frontend lives in the separate `blowball-frontend` repo.)

## Common commands

All backend commands run from the repository root.

### Backend

The server and the seed CLI are a single cobra binary `bin/blowball` with `serve` and `seed` subcommands sharing persistent `-f`/`--config` (config path, default `config.yaml`) and `-d`/`--data-dir` (runtime root, default `.`) flags. The runtime root holds `data/`, `logs/`, and `skills/`; with the default `-d .` these resolve to `./data`, `./logs`, `./skills`.

```bash
# Build the unified blowball binary (serve + seed subcommands)
make build

# Run the server (builds first): ./bin/blowball serve
make run

# Run the server under a dedicated runtime root (-d) and config (-f)
./bin/blowball serve -d /var/lib/blowball -f /etc/blowball/config.yaml

# Select a process role (default --role all == the pre-split monolith on server.port).
# Run the two split roles against the SAME -d root so they share the data plane:
./bin/blowball serve --role api   -d /var/lib/blowball -f /etc/blowball/config.yaml   # CRUD on server.port (8080)
./bin/blowball serve --role agent -d /var/lib/blowball -f /etc/blowball/config.yaml   # streaming + /mcp/tools on server.agent_port (8081)
# Rollback: stop both role processes and run plain `serve` (== --role all) on server.port.

# Create a user (password is prompted securely)
./bin/blowball seed --username alice
./bin/blowball seed --username alice --password 's3cret' --dry-run   # preview hash only

# Run all Go tests with race detection
make test

# Run a single package's tests
go test ./internal/agent/...

# Run a single test
go test ./internal/agent/ -run TestConfuciusDispatchesSubAgent

# Run integration tests (uses fakes for MySQL/LLM, real orchestrator + handlers)
go test ./test/integration/...

# Static analysis
make lint

# Clean build artifacts
make clean
```

### Frontend

The frontend lives in the separate `blowball-frontend` repo (sibling directory); run `npm install` / `npm run dev` / `npm run build` there. See that repo's README.

### Local development environment

```bash
# Start MySQL + Redis (auto-runs migrations in migrations/)
docker compose up -d

# Create config and set secrets
cp config.example.yaml config.yaml
# edit config.yaml: openai.api_key, jwt.secret, mysql/redis credentials

# Create a user (password is prompted securely)
make build
./bin/blowball seed --username alice

# Run server
make run
```

### Executor tools (Linux only)

The `bash`, `python` and `pip_install` tools require [bubblewrap](https://github.com/containers/bubblewrap) (`bwrap`) and an environment that allows unprivileged user namespaces. They are automatically skipped on macOS and Windows. To enable them on Linux:

```bash
# Install bubblewrap (Debian/Ubuntu example)
sudo apt install bubblewrap

# Enable in config.yaml
# tools:
#   executor:
#     bash:
#       enabled: true
#     python:
#       enabled: true
#     pip:
#       enabled: true
#       # pip defaults to network: true because it needs to reach PyPI.
#       # index_url: https://pypi.tuna.tsinghua.edu.cn/simple
#       # trusted_hosts:
#       #   - pypi.tuna.tsinghua.edu.cn
```

`pip_install` runs `python3 -m pip install --target /workspace/.pip` inside the sandbox and adds `/workspace/.pip` to `PYTHONPATH`, so packages installed by the agent are immediately available to the `python` tool without manipulating `sys.path`. If an executor tool is enabled but `bwrap` is missing, the server exits with a fatal error. Keep `enabled: false` (the default) to run without bubblewrap.

The runtime root (`-d`) holds a fourth subdir `{data-dir}/tools`, created at startup alongside `data`/`logs`/`skills`. Operators drop CLI binaries (e.g. `node`, custom CLIs) here to expose them inside the `bash`/`python`/`pip_install` sandboxes: the directory is mounted read-only at the in-sandbox `$HOME/.local/bin` (`/home/blowball/.local/bin`), and `$HOME/.local/bin` is prepended to `PATH`. The directory is optional content-wise (an empty dir binds harmlessly), and there is no API to manage it — operators place files directly on disk. See the `internal/tool/executor/` note above for the full sandbox `$HOME`/`PATH` behavior.

## High-level architecture

### Backend request flow

The unified binary lives in `cmd/blowball/`: `main.go` wires the cobra root (`serve`/`seed` subcommands + persistent `-f`/`-d` flags), `serve.go` holds the server bootstrap (`serveRun`), and `seed.go` holds the user-creation subcommand.

The `serve` subcommand bootstraps the application in a strict sequence: resolve `--role`/`-f`/`-d` → shared setup (`setupRuntime`: load config (`-f`, with `${VAR}` env expansion) → derive `dataDir`/`logDir`/`skillsDir`/`toolsDir` from `-d` → `MkdirAll({d}/logs)` → initialize the zap logger with a role-scoped filename (tee console + file under `{d}/logs/blowball[-api|-agent].log`, rotated by lumberjack) → enforce the role-aware `openai.api_key` requirement → connect MySQL/Redis → create the filesystem store under `{d}/data` → `MkdirAll({d}/skills)` → `MkdirAll({d}/tools)` → apply Landlock sandboxing (read-write for `data`/`logs`/`skills`, read-only for `tools`; Linux-only)) → build the shared `SessionService` → register routes by role via `wireAPI`/`wireAgent` → start a gracefully-shutdown HTTP listener. The log directory is created and the log file opened before the logger emits its first line, and before Landlock is applied, so lumberjack's post-rotation reopen stays inside the sandbox.

### Deployment roles (`--role`)

`serve` selects a process role via `--role all|api|agent` (default `all`). The role decides which route partition the process registers, which listener it opens, and which log file it writes. This lets agent execution run as an independent process so a surge or crash in the agent path cannot take the CRUD API down, while the data plane stays shared and unchanged (Phase 1: same-machine, shared local filesystem; cross-host scaling and object storage are out of scope).

| Role | Routes registered | Listener | Log file | Wires |
|------|-------------------|----------|----------|-------|
| `all` (default) | full set (API ∪ agent) on one engine | `server.port` (default 8080) | `blowball.log` | everything — identical to the pre-split monolith (dev, tests, rollback) |
| `api` | CRUD only: auth, session CRUD, message-history read, title update, workspace file CRUD, skills list | `server.port` | `blowball-api.log` | `wireAPI` — CRUD services/handlers only; **no** orchestrator, OpenAI client, tool registry, or MCP manager. A missing `openai.api_key` is a warning, not fatal (the api role never calls the LLM). |
| `agent` | streaming message endpoint (`POST /sessions/:id/messages`) + `GET /api/v1/mcp/tools` (both JWT-authed) | `server.agent_port` (default 8081) | `blowball-agent.log` | `wireAgent` — tool registry, MCP manager, OpenAI client, orchestrator, `TitleService`, and the streaming handler; owns the full streaming-turn pipeline (lookup → recover → orchestrate → SSE → persist → title). |

Route registration is partitioned in `internal/handler/router.go`: `RegisterAPIRoutes` / `RegisterAgentRoutes` each mount their own subset behind `AuthMiddleware` (middleware order is Recovery → Trace → CORS → Auth on every engine); `RegisterRoutes` is the union used by the `all` role and the integration test harness. The streaming handler (`MessageStreamHandler`, `internal/handler/message_stream.go`) is wired only by the agent role; the api role's `SessionHandler` is CRUD-only and takes no orchestrator, so the api process has a compile-time-honest boundary against the agent layer.

**Shared data plane:** both roles derive `data`/`logs`/`skills`/`tools` from the same `-d` root and connect the same MySQL/Redis/local FS — the three-layer persistence, `xizhi` workspace tools, executor sandbox, and Landlock policy behave exactly as in the monolith. A turn persisted by the agent role is readable by the api role through the shared store. When `storage.workspace.backend: shared` (see `workspace-shared-storage`), the `data` subtree is a shared POSIX filesystem (operator-mounted, MinIO-backed JuiceFS), so the data plane is shared **across machines** too — api/agent instances on different hosts read/write one per-user data tree over the network FS, with all POSIX file operations transparent and unchanged.

**External request routing is out of scope.** How traffic reaches each role's port (reverse proxy, gateway, ingress) is operator-managed; blowball only listens on the role's port and logs `role`, `port`, and the registered route groups at startup. Pointing the streaming route at the api port (or running two `api` roles) surfaces as 404s at the routing layer — by design.

HTTP routes live in `internal/handler/router.go`. Protected routes use `middleware.AuthMiddleware` (JWT Bearer validation) after `TraceMiddleware` and CORS. Key endpoints:

| Method | Path | Notes |
|--------|------|-------|
| `POST` | `/api/v1/auth/login` | Public; returns JWT. |
| `GET`  | `/api/v1/sessions` | List sessions. |
| `POST` | `/api/v1/sessions` | Create session. |
| `GET`  | `/api/v1/sessions/:session_id/messages` | Paginated history. |
| `POST` | `/api/v1/sessions/:session_id/messages` | Send a message; returns SSE stream. |
| `DELETE` | `/api/v1/sessions/:session_id` | Archive + purge; 404 if missing/non-owner. |
| `GET`  | `/api/v1/workspace/files` | List workspace files. |
| `POST` | `/api/v1/workspace/upload` | Multipart upload. |
| `GET`  | `/api/v1/workspace/files/*path` | Download file. |
| `GET`  | `/api/v1/workspace/files/*path/content` | Read file text content. |
| `GET`  | `/api/v1/workspace/files/download/*path?token=<jwt>` | Token-authenticated download for browser-native elements. |
| `DELETE` | `/api/v1/workspace/files/*path` | Delete file or directory. |
| `GET`  | `/api/v1/mcp/tools` | List discovered MCP tools. |
| `GET`  | `/api/v1/skills` | List skills visible to the authenticated user. |
| `GET`  | `/healthz` | Unauthenticated health check. |

See `api/openapi.yaml` for full request/response schemas.

A chat request flows: `MessageStreamHandler.SendMessage` (agent role) → `MessageService.RecoverMessages` (load history) + `AppendMessage` → `OrchestratorRunner.Handle` → SSE writer `stream.WriteSSE`. Title generation runs asynchronously after the first assistant response.

### Workspace file routing

Because gin does not allow a static `/download` segment alongside a `/*path` wildcard at the same tree node, workspace file GET routes share a single catch-all at `/api/v1/workspace/files/*path`. The handler dispatches internally:

- `.../files/download/*path` → `WorkspaceHandler.TokenDownload` (query-token auth via `QueryTokenAuthMW`).
- `.../files/*path/content` → `WorkspaceHandler.Content` (text content; rejects binary files).
- `.../files/*path` → `WorkspaceHandler.Download` (header auth).

DELETE uses the same wildcard pattern under a different method. The token-download endpoint exists so browser-native elements (`<a download>`, `<img>`, PDF.js) can access workspace files without custom `Authorization` headers.

### Agent orchestration

The agent layer is in `internal/agent/`.

- `Agent` interface: `Run(ctx, messages, hub)` returns usage.
- `LLMClient` interface: `StreamChat(ctx, req, onToken)`.
- `OpenAIClient` (`openai_client.go`) implements `LLMClient` with `openai-go/v3`, structured debug logging, and a `toolCallStitcher` that reassembles fragmented tool-call deltas.
- `Orchestrator` (`orchestrator.go`) is the per-request entry point. It builds a fresh agent graph via `AgentFactory.Build`, runs `Confucius`, and emits the final `done` event with aggregated token usage.

Three agents are configured in `config.yaml`:

- `Confucius` — central orchestrator. Runs a tool-calling loop and can dispatch `invoke_chongzhi` / `invoke_liang` sub-agent calls.
- `Chongzhi` — coding agent with workspace file tools (`xizhi_*`).
- `Liang` — analysis agent without file tools.

`Confucius` dispatches tool calls in parallel. Sub-agents receive only the `task` and `context` passed by `Confucius`, stream through the same `stream.Hub`, and cannot recursively invoke other agents. Round limits are hard-coded in the agent implementations.

### Tools, MCP, and skills

Tools are registered in a process-wide `tool.Registry` (`internal/tool/registry.go`). The registry resolves configured tool names to `*ToolSpec` values and renders the OpenAI `tools[]` shape.

Built-in tool families:

- `internal/tool/xizhi/` — workspace file tools (`xizhi_read_file`, `xizhi_write_file`, `xizhi_modify_file`, `xizhi_list_files`, `xizhi_tree`, `xizhi_glob_files`). Each closure is scoped to the requesting user's workspace root (`data/{userID}/workspace`). `validatePath` rejects absolute paths, `..`, and symlink escapes. `modify_file` requires a unique old-content match. Landlock provides defense-in-depth on Linux.
- `internal/tool/webfetch/` — `webfetch` HTTP fetch tool.
- `internal/tool/executor/` — sandboxed command execution (`bash`, `python`, `pip_install`). Only available on Linux when `bwrap` (bubblewrap) is installed. Each invocation runs in a fresh user/mount/pid/network namespace, binds `data/{userID}/workspace` to `/workspace`, clears the environment and re-injects only variables matching `allowed_env_patterns`, and subjects commands to a configured timeout and `max_output_bytes` cap. Network access is disabled by default for `bash` and `python` (`--unshare-net`) but enabled by default for `pip_install`. Installed packages are written to `/workspace/.pip` and exposed to the `python` tool through `PYTHONPATH`. Every execution emits an audit log entry; dangerous keywords (`rm`, `curl`, `wget`, `sudo`, `sshd`) trigger a warning log but do not block execution. The sandbox also establishes a real writable `$HOME` as an in-namespace tmpfs (forced to the synthetic `/home/blowball` regardless of `allowed_env_patterns`, so a leaked host `HOME` never points into the void), binds the operator tools dir `{data-dir}/tools` read-only onto `$HOME/.local/bin`, and prepends `$HOME/.local/bin` to `PATH` so operator-provided CLIs are invocable by bare name and take precedence over host `/usr/bin`. Known limitation: tools that resolve the home directory via `getpwuid(getuid())` instead of the `HOME` env var still see the host uid's `/etc/passwd` entry (bound read-only from the host); a synthetic `/etc/passwd` override is deferred until a real tool proves to need it.
- `internal/tool/luban/` — skill management tools: `luban_list_skills`, `luban_read_skill`, and `luban_install_skill`. `luban_install_skill` `git clone`s a GitHub repo (`--depth 1`) or downloads a single `SKILL.md` (≤500KB) into the requesting user's `data/{userID}/skills/` dir. These are registered only when an agent's config explicitly lists one of them (`needsLubanTools` in `cmd/blowball/serve.go`).
- `internal/tool/skill/` — legacy `read_skill` loader plus the shared `skill.Loader` (used by luban). `read_skill` is registered only for backward compatibility when an agent explicitly lists it; new configs should use `luban_read_skill`.
- `internal/tool/mcpclient/` — external MCP client. Supports `sse`, `stdio`, and Streamable `http` transports. Discovered tools are registered with an optional prefix to avoid collisions.

Agent tool visibility is strictly configured:

- `agents.<name>.tools` lists built-in tools the agent may use.
- `agents.<name>.mcp.servers` grants access to specific MCP servers/tools (`["*"]` for all tools from that server).
- `agents.<name>.skills` lists skill names injected into the system prompt and enables `read_skill`/`luban_read_skill`.
- `agents.<name>.thinking` enables OpenAI reasoning mode (o1/o3/o4-mini/GPT-5 variants): `max_tokens` is sent as `max_completion_tokens` and `reasoning_effort` (`low`/`medium`/`high`) is included. `reasoning_effort` may only be set when `thinking: true` — config validation in `internal/config/config.go` rejects it otherwise.

Skills are `{skill-name}/SKILL.md` files with YAML frontmatter (`name`, `description`). Global skills live in `{data-dir}/skills/` (default `./skills/`); per-user skills live in `data/{userID}/skills/`. User skills override global skills of the same name.

### SSE streaming

`internal/stream/event.go` defines `StreamEvent` and event types: `agent_start`, `token`, `reasoning`, `tool_call`, `tool_result`, `agent_end`, `agent_error`, `done`. `reasoning` carries thinking-mode chunks (emitted when an agent runs with `thinking: true`). `message` is a sentinel type used only for persisted user-message rows; it is never emitted over SSE.

- `stream.Hub` (`hub.go`) is a single-consumer buffered channel. Agents produce events via `Send`/`SendCtx`; `Send` is non-blocking and drops on full buffer.
- `stream.WriteSSE` (`sse.go`) consumes the hub and writes `event:` + `data:` SSE frames to the HTTP response, flushing after each event.
- The hub decouples producers from the HTTP writer so slow clients do not block agent loops.

### Persistence

Messages and sessions use a three-layer write path centered in `internal/service/session.go` (`SessionService.SaveMessagesBatch`):

1. Redis (`internal/store/redis/`) — hot cache; keys `session:{id}` and `msgs:{id}` with TTL.
2. Filesystem (`internal/store/fs/`) — warm tier; per-user JSON files under `data/{userID}/sessions/{sessionID}.json`.
3. MySQL (`internal/store/mysql/`) — source of truth; users, sessions, titles, messages.

Writes to Redis are best-effort; writes to FS are synchronous; writes to MySQL are synchronous but errors are swallowed so SSE streaming never blocks. `MessageService.RecoverMessages` reads Redis first, falls back to FS, then MySQL, and backfills faster tiers.

`internal/store/mysql/message.go` implements cursor-based pagination with a composite cursor `(msg_time, msg_index, id)` clamped to `[1, 200]` items per page.

Each message row carries an `event_type` column. Reasoning/thinking output is persisted as ordinary rows with `event_type='reasoning'` (no separate column), so it survives reloads and is replayed into multi-turn context. SQL migrations live in `migrations/`; `docker compose` mounts the whole directory into MySQL's `/docker-entrypoint-initdb.d`, so files run alphabetically on first init. `migrations/008_deletion_archive.sql` creates `*_deleted` mirror tables (`users_deleted`, `sessions_deleted`, `titles_deleted`, `messages_deleted`) that archive rows verbatim before a session is purged — `SessionService.DeleteSession` copies the session/titles/messages into them in a single transaction, then deletes the live `sessions` row (cascade clears titles/messages) and removes the warm-tier FS file. The mirrors carry no foreign keys and `messages_deleted.id` is a plain `BIGINT` preserving the source id; `users_deleted` is scaffolding, not yet written. Redis cache is intentionally not cleared on delete (reads re-validate ownership against MySQL first, so stale keys are unreachable until TTL).

### Frontend

The React 19 + Vite + TypeScript frontend lives in the separate `blowball-frontend` repo. The API contract is defined here in `api/openapi.yaml`; when it changes, copy that file into `blowball-frontend/` and run `npm run generate-api` there to regenerate `src/lib/openapi.d.ts`.

## Important conventions

- **Config**: `internal/config/config.go` loads YAML and expands `${VAR}` / `${VAR:default}` from the environment. Durations support short suffixes (`s`, `m`, `h`, `d`, `w`).
- **Workspace storage backend**: `storage.workspace.backend` (`internal/config/config.go`, `StorageConfig`) selects where the per-user data root `{data-dir}/data` physically lives: `local` (default, each process's local disk) or `shared` (operator-mounted shared POSIX filesystem — MinIO-backed JuiceFS). Default is zero-behavior-change. In `shared` mode `setupRuntime` runs a startup health check (`internal/storage`, before Landlock) that verifies `{data-dir}/data` is writable and FUSE-family, refusing to boot otherwise so a node can never silently degrade to local disk and diverge from the cluster; when executor tools are configured on an agent/all role, `internal/tool/executor.ProbeFUSEWorkspace` additionally self-checks that the mount allows a user-namespace-mapped uid (i.e. JuiceFS `--allow-other`). All POSIX file ops (`xizhi_*`, `WorkspaceHandler`, `fs.Store`, executor `--bind`, `onlyOfficePersist` rename) are unchanged in shared mode — only the mount point changes. See `docs/shared-storage.md` for the deploy/migrate/backup runbook.
- **Context values**: `TraceMiddleware` mints `trace_id`; `AuthMiddleware` injects `userID`. Both propagate through stores via context. The skill tool reads `userID` from context to scope skill lookups.
- **Not-found handling**: MySQL and filesystem store methods return `(nil, nil)` on missing records, not errors.
- **Security**: there is no public user-creation endpoint; users are created via the `blowball seed` subcommand. Workspace file tools enforce per-user path scoping at the application layer; Landlock is a best-effort extra layer on Linux. Landlock is scoped to the four runtime subdirs — read-write for `{d}/data`, `{d}/logs`, `{d}/skills` (the logs dir is covered for lumberjack's post-rotation reopen) and read-only for the operator tools dir `{d}/tools` (mirroring the in-sandbox `--ro-bind`); in production point `-d` at a dedicated directory rather than the repo root to keep the sandbox tight. In `storage.workspace.backend: shared` mode the per-user data subtree lives on a shared FUSE mount (JuiceFS) that must be mounted with `--allow-other` (and `/etc/fuse.conf` `user_allow_other`) so the bwrap sandbox's mapped uid can access `/workspace`; `validatePath` symlink defense, path scoping, and the bwrap namespace all remain in force, and access is still gated by JWT auth, so the wider FUSE uid access is bounded to authenticated, path-validated operations on a controlled server host.
- **Login policy**: `auth.password_required` (in `internal/config/config.go`, `AuthConfig`) toggles whether `POST /api/v1/auth/login` verifies the password. It defaults to `true` (bcrypt verified against the seed-created hash). Set `auth.password_required: false` for passwordless login — `AuthService.Login` skips bcrypt, so any seeded, active user logs in by username alone (a sent password is ignored). The flag is threaded into `service.NewAuthService` from `cfg.Auth.IsPasswordRequired()` in `wireAPI`; the handler and frontend are mode-agnostic.
- **Prompt rendering**: `internal/prompt/render.go` assembles the system prompt with environment info, built-in tools, MCP tools grouped by server, and skills as XML tags.
- **Message reconstruction**: `internal/handler/message_reconstruct.go` rebuilds agent-ready conversation history from persistence, tracking tool-call state across messages.
- **Testing**: unit tests are per-package; integration tests in `test/integration/` exercise real handlers/services/orchestrator with faked MySQL, Redis, and LLM.
