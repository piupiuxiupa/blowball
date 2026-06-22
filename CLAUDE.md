# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Blowball is a Go backend for a multi-agent chat workspace with a React frontend. It exposes a JWT-secured HTTP API (Gin), persists sessions/messages in MySQL with Redis caching and filesystem warm storage, and orchestrates OpenAI-backed agents.

## Common commands

All backend commands run from the repository root. Frontend commands run from `frontend/`.

### Backend

```bash
# Build the server and seed CLI
make build

# Run the server (builds first)
make run

# Run all Go tests with race detection
make test

# Run a single package's tests
go test ./internal/agent/...

# Run a single test
go test ./internal/agent/ -run TestConfuseDispatchesSubAgent

# Run integration tests (uses fakes for MySQL/LLM, real orchestrator + handlers)
go test ./test/integration/...

# Static analysis
make lint

# Clean build artifacts
make clean
```

### Frontend

```bash
cd frontend

# Install dependencies
npm install

# Start Vite dev server (proxies /api to localhost:8080)
npm run dev

# Type-check and build
npm run build

# Type-check only
npm run lint

# Regenerate TypeScript types from ../api/openapi.yaml
npm run generate-api
```

### Local development environment

```bash
# Start MySQL + Redis (auto-runs migrations in migrations/)
docker compose up -d

# Create config and set secrets
cp config.example.yaml config.yaml
# edit config.yaml: openai.api_key, jwt.secret, mysql/redis credentials

# Create a user (password is prompted securely)
make build
./bin/seed -username alice

# Run server
make run
```

## High-level architecture

### Backend request flow

`cmd/server/main.go` bootstraps the application in a strict sequence: load `config.yaml` (with `${VAR}` env expansion), initialize the zap logger, connect MySQL/Redis, create the filesystem store under `data/`, apply Landlock sandboxing (Linux-only), register tools, build services, build the orchestrator, construct handlers, wire the Gin router, and start a gracefully-shutdown HTTP server.

HTTP routes live in `internal/handler/router.go`. Protected routes use `middleware.AuthMiddleware` (JWT Bearer validation) after `TraceMiddleware` and CORS. Key endpoints:

- `POST /api/v1/auth/login` — returns a JWT.
- `GET|POST /api/v1/sessions` — list/create sessions.
- `GET /api/v1/sessions/:session_id/messages` — paginated history.
- `POST /api/v1/sessions/:session_id/messages` — send a message, returns SSE stream.
- `GET|POST /api/v1/workspace/*` — list/upload/download/read workspace files.
- `GET /api/v1/mcp/tools` — list discovered MCP tools.
- `GET /api/v1/skills` — list skills visible to the authenticated user.

A chat request flows: `SessionHandler.SendMessage` → `MessageService.RecoverMessages` (load history) + `AppendMessage` → `OrchestratorRunner.Handle` → SSE writer `stream.WriteSSE`. Title generation runs asynchronously after the first assistant response.

### Agent orchestration

The agent layer is in `internal/agent/`.

- `Agent` interface: `Run(ctx, messages, hub)` returns usage.
- `LLMClient` interface: `StreamChat(ctx, req, onToken)`.
- `OpenAIClient` (`openai_client.go`) implements `LLMClient` with `openai-go/v3`, structured debug logging, and a `toolCallStitcher` that reassembles fragmented tool-call deltas.
- `Orchestrator` (`orchestrator.go`) is the per-request entry point. It builds a fresh agent graph via `AgentFactory.Build`, runs `Confuse`, and emits the final `done` event with aggregated token usage.

Three agents are configured in `config.yaml`:

- `Confuse` — central orchestrator. Runs a tool-calling loop and can dispatch `invoke_chongzhi` / `invoke_liang` sub-agent calls.
- `Chongzhi` — coding agent with workspace file tools (`xizhi_*`).
- `Liang` — analysis agent without file tools.

`Confuse` dispatches tool calls in parallel. Sub-agents receive only the `task` and `context` passed by `Confuse`, stream through the same `stream.Hub`, and cannot recursively invoke other agents. Round limits are hard-coded in the agent implementations.

### Tools, MCP, and skills

Tools are registered in a process-wide `tool.Registry` (`internal/tool/registry.go`). The registry resolves configured tool names to `*ToolSpec` values and renders the OpenAI `tools[]` shape.

Built-in tool families:

- `internal/tool/xizhi/` — workspace file tools (`xizhi_read_file`, `xizhi_write_file`, `xizhi_modify_file`, `xizhi_list_files`, `xizhi_tree`, `xizhi_glob_files`). Each closure is scoped to the requesting user's workspace root (`data/{userID}/workspace`). `validatePath` rejects absolute paths, `..`, and symlink escapes. `modify_file` requires a unique old-content match. Landlock provides defense-in-depth on Linux.
- `internal/tool/webfetch/` — `webfetch` HTTP fetch tool.
- `internal/tool/skill/` — skill discovery and the `read_skill` on-demand skill loader.
- `internal/tool/mcpclient/` — external MCP client. Supports `sse`, `stdio`, and Streamable `http` transports. Discovered tools are registered with an optional prefix to avoid collisions.

Agent tool visibility is strictly configured:

- `agents.<name>.tools` lists built-in tools the agent may use.
- `agents.<name>.mcp.servers` grants access to specific MCP servers/tools (`["*"]` for all tools from that server).
- `agents.<name>.skills` lists skill names injected into the system prompt and enables `read_skill`.

Skills are `{skill-name}/SKILL.md` files with YAML frontmatter (`name`, `description`). Global skills live in `./skills/`; per-user skills live in `data/{userID}/skills/`. User skills override global skills of the same name.

### SSE streaming

`internal/stream/event.go` defines `StreamEvent` and event types: `agent_start`, `token`, `tool_call`, `tool_result`, `agent_end`, `agent_error`, `done`.

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

### Frontend

The frontend is a React 19 + Vite + TypeScript app in `frontend/`.

- Routing: `react-router` v7 in `src/App.tsx`; `/login` and `/` (protected by `AuthGuard`).
- State:
  - Zustand `auth-store` persists the JWT in `localStorage`.
  - Zustand `ui-store` holds transient UI state (active session/file, streaming tokens, agent status).
  - TanStack Query caches server state (sessions, messages, workspace files).
- API: `src/lib/api.ts` reads `VITE_API_BASE_URL` and injects the bearer token. `src/lib/sse.ts` parses SSE streams.
- Hooks in `src/hooks/` are the only place components should call the API.
- Streaming: `useSendMessage` dispatches SSE events into `ui-store`; `message-list.tsx` groups raw events into logical assistant/user blocks.
- Workspace files: `useWorkspace` lists files; `file-renderer.tsx` dispatches by extension to markdown/code/image/PDF/binary viewers.
- Styling: Tailwind CSS v4 with a single light theme; minimal hand-built UI component subset in `src/components/ui/`.
- Types: generated from `../api/openapi.yaml` via `npm run generate-api` into `src/lib/openapi.d.ts`.

Vite dev server proxies `/api` to `http://localhost:8080`.

## Important conventions

- **Config**: `internal/config/config.go` loads YAML and expands `${VAR}` / `${VAR:default}` from the environment. Durations support short suffixes (`s`, `m`, `h`, `d`, `w`).
- **Context values**: `TraceMiddleware` mints `trace_id`; `AuthMiddleware` injects `userID`. Both propagate through stores via context. The skill tool reads `userID` from context to scope skill lookups.
- **Not-found handling**: MySQL and filesystem store methods return `(nil, nil)` on missing records, not errors.
- **Security**: there is no public user-creation endpoint; users are created via `cmd/seed`. Workspace file tools enforce per-user path scoping at the application layer; Landlock is a best-effort extra layer on Linux.
- **Prompt rendering**: `internal/prompt/render.go` assembles the system prompt with environment info, built-in tools, MCP tools grouped by server, and skills as XML tags.
- **Message reconstruction**: `internal/handler/message_reconstruct.go` rebuilds agent-ready conversation history from persistence, tracking tool-call state across messages.
- **Testing**: unit tests are per-package; integration tests in `test/integration/` exercise real handlers/services/orchestrator with faked MySQL, Redis, and LLM.
