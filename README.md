# blowball

A Go backend for a multi-agent chat workspace. It exposes a JWT-secured HTTP API built with [Gin](https://gin-gonic.com/), persists sessions and messages in MySQL (Redis is a read cache), and orchestrates LLM agents backed by any OpenAI-compatible gateway. Scheduling runs in a separate scheduler binary. The React frontend lives in the separate [`blowball-frontend`](../blowball-frontend) repo.

## Features

- **JWT authentication** with bcrypt-hashed passwords; users are created via a `seed` CLI (no public sign-up).
- **Session management** — create, list, update titles (manual or AI-generated), fetch paginated message history, delete (archived to mirror tables). Optional per-session FIFO message queue so a busy session admits and auto-continues instead of returning 409.
- **Server-Sent Events (SSE)** streaming with fine-grained event types — `agent_start`, `token`, `reasoning`, `tool_call`, `tool_result`, `agent_end`, `agent_error`, `artifact`, `done` — plus detach/resume: a turn survives client disconnects and its stream can be replayed by run id.
- **Agent orchestration** — a root `Confucius` agent runs a tool-calling loop with parallel tool dispatch, a semantic plan tool, and **dynamic sub-agents**: every spawn goes through the synthetic `spawn_subagent` tool and builds a fresh generic sub-agent from a task prompt + narrowed tool set, bounded by tree budgets (depth, concurrency, total per turn).
- **Per-request model selection** — a mandatory `openai.models` catalog (context window, completion quota, `thinking`, `vision`, optional length-continuation) with model + reasoning effort chosen per request; one model/quota per turn across the whole spawn tree.
- **Per-user LLM tokens** — each user can configure their own gateway token (masked in all responses); unset users share the deployment key.
- **Long-context support** — automatic context compaction into merging checkpoint summaries at ~80% of the input budget, with byte-identical prefix stability for provider prompt caching; optional `finish_reason=length` continuation; optional stream idle watchdog.
- **Cross-session memory** (optional, external [OpenViking](https://github.com/volcengine/OpenViking)) — turn-start recall injected into context, turn-end capture as a background side effect.
- **Turn artifacts** — files the agent links with `blowball://workspace/...` in its final reply are version-snapshotted and announced to the frontend, so history opens "the content as it was then".
- **Image attachments** — magic-byte-sniffed uploads (png/jpeg/webp/gif) on user messages, inlined into the current turn for `vision: true` models.
- **Scheduled agent tasks** — RFC3339 `at` and five-field `cron` tasks (IANA zones) running agent turns headlessly, owned by a standalone scheduler binary over authenticated internal gRPC; task conversations live in a separate read-only session store.
- **Workspace file tools** (`xizhi_*`) scoped per user: read, write, modify, list, tree, find, grep, plus `webfetch` and the opt-in image-understanding sidecar `xizhi_view_image` (a vision model describes a workspace image; the image never enters the conversation).
- **OnlyOffice integration** — edit workspace documents in the browser via a DocumentServer, with historical version rendering.
- **Sandboxed command execution** (`bash`) via bubblewrap on Linux.
- **Skill management** (`luban_*`) — list, read, install, and browse skills from GitHub or a remote `SKILL.md`; optional remote skill market.
- **MCP support** — operator-level servers (SSE / stdio / Streamable HTTP) proxied into the tool catalogue, plus per-user turn-scoped MCP servers with their own credentials and an optional MCP market.
- **Split process roles** — run `--role api` (CRUD) and `--role agent` (streaming) as separate processes sharing one data plane; optional shared JuiceFS storage backend for multi-node deployments.
- **Graceful shutdown**, structured JSON logging with zap, raw LLM request/response capture for post-mortems, per-turn usage tracking, and an OpenAPI 3 spec at [`api/openapi.yaml`](api/openapi.yaml).

## Quick start

### 1. Requirements

- Go 1.26+
- MySQL 8.0
- Redis 7
- An OpenAI-compatible API key

### 2. Start dependencies

```bash
docker compose -f deploy/compose/mysql-redis.yaml up -d
```

This starts MySQL on `3306` and Redis on `6379`, and auto-runs the SQL migrations in [`migrations/`](migrations/) on a fresh volume (existing databases apply new migrations manually).

### 3. Configure

Copy the example config and fill in your secrets:

```bash
cp config.example.yaml config.yaml
```

At minimum set:

```yaml
openai:
  api_key: ${OPENAI_API_KEY}
  models:            # the mandatory model catalog — see the example file
    - name: gpt-5
      max_context_tokens: 400000
      max_completion_tokens: 16384
      thinking: true
      vision: true

jwt:
  secret: ${JWT_SECRET}
```

All values support `${VAR}` and `${VAR:default}` environment substitution.

### 4. Create a user

The API has no public sign-up endpoint. Use the seed subcommand:

```bash
make build
./bin/blowball seed --username alice
```

You will be prompted for a password. The tool stores a bcrypt hash and prints the generated `user_id`. Add `--password 'pw'` to go non-interactive, or `--dry-run` to preview the hash without writing.

**Passwordless login (optional).** Set `auth.password_required: false` in `config.yaml` to skip password verification at login — any seeded, active user can then log in by username alone. The default is `true` (password required).

### 5. Run the server

```bash
make run        # = ./bin/blowball serve
```

The server listens on the port configured in `config.yaml` (default `8080`). It is a single cobra binary with two subcommands (`serve`, `seed`) sharing the persistent flags `-f`/`--config` (config path, default `config.yaml`) and `-d`/`--data-dir` (runtime root, default `.`):

```bash
./bin/blowball serve                                     # reads ./config.yaml, writes ./data, ./logs
./bin/blowball serve -d /var/lib/blowball -f /etc/bb/config.yaml   # gather all runtime state under one root
```

The runtime root holds `data/` (per-user workspaces, attachments, per-user skills), `logs/` (the rotated structured log), `skills/` (global skills), `tools/` (operator CLIs exposed inside the bash sandbox), `skills-market/` and `mcp-market/` (operator-synced market payloads). Missing directories are created on startup.

For production you can split the process into independent roles (see [Deployment roles](#deployment-roles)), and run the standalone scheduler if you use scheduled tasks (see [Scheduled agent tasks](#scheduled-agent-tasks)).

### 6. Run the frontend (optional)

The React frontend lives in the separate `blowball-frontend` repo. From there, run `npm install` then `npm run dev`; the Vite dev server starts on port `5173` and proxies `/api` to `http://localhost:8080`.

## Deployment roles

`serve` selects a process role via `--role all|api|agent` (default `all` — the single-process monolith):

| Role | Routes | Listener | Log file |
|------|--------|----------|----------|
| `all` | full set (API ∪ agent) | `server.port` (default 8080) | `blowball.log` |
| `api` | CRUD: auth, sessions, history, queue, attachments, workspace, skills, models, llm-token, scheduled-task management | `server.port` | `blowball-api.log` |
| `agent` | streaming message endpoint, turn lifecycle, `/api/v1/mcp/tools` | `server.agent_port` (default 8081) | `blowball-agent.log` |

Both roles must run against the **same** `-d` runtime root and MySQL/Redis so they share the data plane; how traffic reaches each port (reverse proxy, gateway, ingress) is operator-managed. The `api` role never constructs an orchestrator or LLM client — a surge or crash in the agent path cannot take the CRUD API down.

## Scheduled agent tasks

Scheduling is **not embedded** in the main service. Deploy the standalone scheduler binary:

```bash
make build-scheduler        # ./bin/scheduler
make run-scheduler          # runs it with services/scheduler/config.yaml
```

The scheduler owns task definitions, cron materialization, claims and leases; the main service keeps the turn engine, task conversations, `.cron` resource versions and execution receipts. They communicate over authenticated internal gRPC configured in the `internal_rpc:` block (mTLS by default, or an explicit token mode for loopback development). With `internal_rpc` disabled the scheduled-task HTTP endpoints return 503 and nothing degrades silently.

`scheduled_tasks.enabled: true` in the main config fails to load — that embedded scheduler was removed. See [`docs/scheduled-tasks.md`](docs/scheduled-tasks.md) for the full deploy/migrate/rollback runbook.

## Development

```bash
# Build the unified blowball binary (serve + seed subcommands)
make build

# Build the standalone scheduler
make build-scheduler

# Run all tests with race detection
make test

# Run a single package's tests
go test ./pkg/agent/...

# Run a single test
go test ./pkg/agent/ -run TestConfucius_CallsSubAgent_ThenSummarizes

# Run integration tests (fakes for MySQL/LLM, real orchestrator + handlers)
go test ./test/integration/...

# Static analysis
make lint

# Regenerate internal gRPC bindings (needs protoc)
make proto

# Clean build artifacts
make clean
```

See [`CLAUDE.md`](CLAUDE.md) for the full architecture and conventions, and [`AGENTS.md`](AGENTS.md) for contributor guidelines.

## Project layout

```
.
├── api/
│   ├── openapi.yaml          # OpenAPI 3 spec (the HTTP contract)
│   ├── proto/                # internal gRPC contracts (scheduler, scheduledruntime)
│   └── gen/go/               # generated Go bindings (make proto)
├── cmd/blowball/             # the cobra binary: main, serve, seed
├── services/scheduler/       # the standalone Scheduler binary
├── pkg/
│   ├── agent/                # Agents, orchestrator, OpenAI client, sub-agents
│   ├── config/               # YAML config loader
│   ├── handler/              # HTTP handlers and router
│   ├── middleware/           # Trace, CORS, auth middleware
│   ├── model/                # Domain models
│   ├── modelselect/          # Model-catalog resolution (per-request model/effort)
│   ├── rpcx/                 # shared gRPC error codes, mTLS, service-identity authz
│   ├── resource/             # .cron draft/publication service (scheduled tasks)
│   ├── run/                  # turn-run registry
│   ├── schedule/             # pure at/cron rules
│   ├── scheduledruntime/     # scheduled-run execution Runtime (receipts, watchdog)
│   ├── service/              # Business logic layer
│   ├── store/                # MySQL, Redis, filesystem stores
│   ├── stream/               # SSE event stream types and hub
│   ├── tool/                 # Tool registry and tool implementations
│   └── ...
├── migrations/               # SQL schema migrations
├── docs/                     # Runbooks (scheduled-tasks, shared-storage, ...)
├── openspec/                 # Per-capability behavior specs and change proposals
├── test/integration/         # Integration tests
├── config.example.yaml       # Example configuration (heavily commented)
├── deploy/                  # 部署工件:每服务 Dockerfile/Jenkinsfile + compose(MySQL+Redis 基础)
└── Makefile                  # Common tasks
```

## API overview

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/auth/login` | Exchange credentials for a JWT |
| `GET/POST/PATCH/DELETE` | `/api/v1/sessions...` | List, create, update title, archive sessions |
| `GET` | `/api/v1/sessions/{id}/messages` | Paginated message history |
| `POST` | `/api/v1/sessions/{id}/messages` | Send a message, stream SSE (model/effort per request) |
| `POST` | `/api/v1/sessions/{id}/turns/{run_id}/cancel` | Cancel a running turn |
| `GET` | `/api/v1/sessions/{id}/turns/{run_id}/events` | Resume/replay a run's event stream |
| `GET` | `/api/v1/sessions/{id}/queue` | List queued messages |
| `POST` | `/api/v1/sessions/{id}/attachments` | Upload a chat image |
| `GET/PUT/DELETE` | `/api/v1/me/llm-token` | Per-user gateway token management |
| `GET/POST/PUT/DELETE` | `/api/v1/workspace/files...` | Workspace file CRUD, upload, download, text content |
| `GET` | `/api/v1/workspace/versions...` | Artifact version resolve + snapshot content |
| `GET` | `/api/v1/mcp/tools` | List discovered MCP tools |
| `GET` | `/api/v1/skills` | List skills (local + market) |
| `GET` | `/api/v1/models` | The selectable model catalog |
| `GET/POST...` | `/api/v1/scheduled-tasks...` | Scheduled-task management and run history |
| `GET` | `/api/v1/scheduled-sessions...` | Read-only task conversation sessions |
| `GET` | `/healthz` | Health check |

See [`api/openapi.yaml`](api/openapi.yaml) for full request/response schemas and examples.

## External MCP servers

Blowball can act as an MCP client, registering tools from external MCP servers so agents can invoke them alongside built-in tools.

To enable it, add an `mcp.servers` section to `config.yaml`:

```yaml
mcp:
  servers:
    - name: remote_search
      transport: sse
      url: http://localhost:3001/sse
      headers:
        Authorization: Bearer ${MCP_TOKEN}
      timeout: 30s
      call_timeout: 30s
      reconnect: true

    - name: local_calculator
      transport: stdio
      command: ./calculator-mcp-server
      args: ["--stdio"]
      env:
        API_KEY: ${LOCAL_API_KEY}
      timeout: 30s
      call_timeout: 30s
      reconnect: true
      prefix: calc_

    - name: remote_http_search
      transport: http
      url: http://localhost:3002/mcp
      headers:
        Authorization: Bearer ${MCP_TOKEN}
      timeout: 30s
      call_timeout: 30s
      reconnect: true
```

Supported transports:

- `sse` — connects over Server-Sent Events + HTTP POST messages.
- `stdio` — spawns a local subprocess and speaks JSON-RPC over stdin/stdout.
- `http` — Streamable HTTP with automatic `Mcp-Session-Id` management and re-initialization on session expiry.

Configuration fields:

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Unique server identifier. |
| `transport` | yes | `sse`, `stdio`, or `http`. |
| `url` | for `sse` / `http` | Server endpoint. |
| `command` | for `stdio` | Executable to spawn. |
| `args` | no | Command-line arguments for `stdio`. |
| `env` | no | Environment variables injected into the `stdio` child process. |
| `headers` | no | HTTP headers sent with every request (SSE / HTTP). |
| `timeout` | no | Connection / initialization timeout (default `30s`). |
| `call_timeout` | no | Per-tool-call timeout (default `30s`). |
| `reconnect` | no | Reconnect (`sse` / `http`) or restart (`stdio`) on failure. |
| `prefix` | no | Prefix applied to every discovered tool name to avoid collisions. |

All string values support `${VAR}` and `${VAR:default}` environment substitution.

### Per-agent MCP permissions

By default an agent sees no MCP tools. Use `agents.<name>.mcp.servers` to grant access:

```yaml
agents:
  confucius:
    mcp:
      servers:
        - name: remote_search
          tools:
            - web_search
            - fetch_url
        - name: remote_http_search
          tools: ["*"]
```

- `tools: ["*"]` allows every tool discovered from that server.
- Only allowed tools appear in the agent's tool list and system prompt.
- Server names must match an entry in the global `mcp.servers` list.
- Concrete tool names (not `"*"`) are validated against the remote `tools/list` result at startup.

### Per-user MCP servers

Independently of the operator catalog, each user can declare their own servers in `{workspaceRoot}/.blowball/mcp/{name}/config.json` and call them through the `mcp_*` tools (`mcp_call`, `mcp_list_servers`, ...). These are turn-scoped: one user's authenticated connection is never served to another user's turn, credentials are injected server-side and redacted from model-visible input/output, and oversized results spill to workspace files instead of the context. An optional `mcp_market:` block adds a remote-authorized market source (workspace entries win on name conflicts). See `config.example.yaml` and the `user-mcp-configuration` / `mcp-market` specs under `openspec/specs/`.

### Security considerations

- **Allowlist only** — only servers declared in `mcp.servers` are connected, and only tools explicitly allowed per agent are exposed to that agent.
- **Auth injection** — use `headers` (SSE / HTTP) and `env` (stdio) for secrets; both support environment substitution so credentials never need to be hard-coded in config.
- **Timeouts** — per-server `timeout` and `call_timeout` prevent a slow or hung remote server from blocking agent turns indefinitely.
- **Remote errors** — failures from an MCP server are surfaced as tool errors in the stream and do not crash the agent loop.

## Skills

Skills are instruction documents stored as `{skill-name}/SKILL.md` with YAML frontmatter:

```markdown
---
name: coding-style
description: Project coding conventions
---

# Coding Style

Always run `gofmt` before finishing Go edits...
```

Blowball discovers skills from three locations:

- Global skills: `{data-dir}/skills/`
- Per-user skills: `data/{userID}/skills/`
- Market skills (optional): `{data-dir}/skills-market/` synced by the operator, authorized per user via the `skill_market:` block

User skills override global skills of the same name; global overrides market.

### Enabling skills for an agent

```yaml
agents:
  confucius:
    skills:
      - coding-style
      - review-checklist
```

When an agent has skills configured, the skill catalogue is injected into its system prompt. Agents load skill instructions on demand via `luban_read_skill(name)` and can browse a skill's files (`luban_list_skill_files`, `luban_tree_skill`) or install new ones from GitHub (`luban_install_skill`). The `luban_*` tools are registered only when an agent explicitly lists them in its `tools`.

### Listing skills

`GET /api/v1/skills` returns the skills visible to the authenticated user:

```json
{
  "skills": [
    {"name": "coding-style", "filename": "coding-style", "size": 1234, "update_time": "2026-06-16T..."}
  ]
}
```

## Sandboxed command execution

On Linux, Blowball can register a `bash` tool that runs commands inside a [bubblewrap](https://github.com/containers/bubblewrap) sandbox. Each invocation gets its own user/mount/pid namespaces, the user's workspace is bound to `/workspace`, and the environment is rebuilt in three tiers: a host allow-list, operator literals from `bash.env`, and forced invariants (`HOME`, `PATH`, `PYTHONPATH`).

```yaml
tools:
  executor:
    bash:
      enabled: true
      timeout: 30s
      max_output_bytes: 65536
      allowed_env_patterns: ["PATH", "HOME", "LANG", "USER", "TERM", "PYTHON*"]
      network: true
      # env:                      # operator-owned literals injected into every sandbox
      #   PIP_INDEX_URL: "https://pypi.mirrors.example.com"
```

- Run Python via `bash` (`python3 ...`); install packages with `python3 -m pip install --target /workspace/.pip <pkg>` — `/workspace/.pip` is on `PYTHONPATH` in every sandbox.
- Network access defaults to **on** so pip can reach PyPI; set `network: false` to tighten.
- Operator CLIs dropped into `{data-dir}/tools` are exposed on the sandbox `$PATH`.
- If the tool is enabled but `bwrap` is not installed, the server exits with a fatal error.
- On macOS and Windows the tool is silently unavailable regardless of config.

## Shared workspace storage (optional, multi-node + DR)

By default each blowball process keeps per-user data (workspaces, attachments, per-user skills) on its **local disk** under `{data-dir}/data`. To share that data across multiple `api`/`agent` instances on different machines and gain object-storage-level disaster recovery, set:

```yaml
storage:
  workspace:
    backend: shared   # local (default) | shared
```

In `shared` mode the operator mounts a **shared POSIX filesystem** — MinIO-backed [JuiceFS](https://juicefs.com/) — onto `{data-dir}/data` **before** starting blowball. All existing POSIX file operations (the `xizhi_*` tools, the workspace HTTP endpoints, the executor `--bind` sandbox, OnlyOffice's atomic save) stay unchanged and transparent; only the mount point differs. On startup, `shared` mode runs a health check that **refuses to boot** if `{data-dir}/data` is not the expected shared mount (preventing a node from silently writing local disk and diverging from the cluster), and — when executor tools are enabled — a bwrap self-check that catches a missing JuiceFS `--allow-other` / `user_allow_other`.

Shared mode is **Linux-only** (JuiceFS, bubblewrap, and Landlock are Linux features); macOS/Windows dev continues to use `backend: local`.

Prerequisites, migration, backup/restore, monitoring, and the rollback path are documented in [`docs/shared-storage.md`](docs/shared-storage.md). A cross-node visibility check is provided in [`scripts/verify-shared-storage.sh`](scripts/verify-shared-storage.sh).

## Configuration

Key sections in `config.yaml` (see `config.example.yaml` for every option, heavily commented):

- `server` — HTTP ports (`port`, `agent_port`).
- `openai` — gateway (`api_key`, `base_url`), the mandatory `models` catalog, `default_model`, `default_reasoning_effort`, `title_model`, and the optional `stream_idle_timeout`.
- `mysql` / `redis` — connection settings.
- `jwt` — signing secret and token expiry (e.g. `7d`).
- `agents` — per-agent system prompts, `tools` lists, MCP permissions, skill lists, `max_rounds`, retry policy, and the `subagent` tree budgets/presets. Agents carry **no** model fields — models live in the `openai.models` catalog.
- `messages` — input token limit (`max_input_tokens`) and the optional per-session queue (`queue.enabled`, `max_depth`).
- `memory` — optional OpenViking cross-session memory.
- `artifact` — turn-artifact snapshot size cap.
- `tools` — enable/disable tool families (xizhi, webfetch, executor) and per-tool timeouts.
- `mcp` — operator MCP server declarations; `mcp_market` — the optional per-user MCP market.
- `skill_market` — the optional remote skill market.
- `internal_rpc` — authenticated gRPC link to the standalone scheduler (mTLS or token mode).
- `storage` — workspace storage backend (`local`, the default, or `shared`); see [Shared workspace storage](#shared-workspace-storage-optional-multi-node--dr).
- `logging` — level; `format` (`json`, the default, or `console`); `output` (sinks, default `["stderr", "file"]`); `file.*` rotation settings for the file sink (`max_size_mb`, `max_backups`, `max_age_days`, `compress`, via lumberjack). The file sink writes `{data-dir}/logs/blowball.log`. Set `output: ["stderr"]` to run console-only with no log file.

## License

MIT
