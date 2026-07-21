# blowball

A Go backend for a multi-agent chat workspace with a React frontend. It exposes a JWT-secured HTTP API built with [Gin](https://gin-gonic.com/), persists sessions and messages in MySQL, caches session state in Redis, and orchestrates a small team of LLM agents backed by OpenAI.

## Features

- **JWT authentication** with bcrypt-hashed passwords.
- **Session management** — create sessions, list them, fetch paginated message history, and delete sessions (deleted sessions are archived to mirror tables).
- **Server-Sent Events (SSE)** streaming for agent responses with fine-grained event types:
  `agent_start`, `token`, `reasoning`, `tool_call`, `tool_result`, `agent_end`, `agent_error`, `done`.
- **Multi-agent orchestration** — a central `Confucius` agent dispatches to specialist agents:
  - `Chongzhi` — coding agent with workspace file tools.
  - `Liang` — analysis and explanation agent.
- **Workspace file tools** (`xizhi_*`) scoped per user: read, write, modify, list, tree, glob, plus `webfetch`.
- **Sandboxed command execution** (`bash`, `python`) via bubblewrap on Linux.
- **Skill management** (`luban_*`) — list, read, and install skills from GitHub or a remote `SKILL.md`.
- **External MCP client support** — connect SSE, stdio, or Streamable HTTP MCP servers at startup and proxy their tools into the agent tool catalogue.
- **Per-agent MCP and skill permissions** — each agent can be restricted to specific MCP servers/tools and skills, and the available set is injected into its system prompt.
- **OpenAI reasoning mode** support (`thinking: true`) for o1/o3/o4-mini/GPT-5 reasoning variants.
- **Graceful shutdown**, structured JSON logging with zap, and OpenAPI 3 spec at [`api/openapi.yaml`](api/openapi.yaml).

## Quick start

### 1. Requirements

- Go 1.26+
- MySQL 8.0
- Redis 7
- An OpenAI API key
- Node.js 20+ (for the frontend)

### 2. Start dependencies

```bash
docker compose up -d
```

This starts MySQL on `3306` and Redis on `6379`, and auto-runs the SQL migrations in [`migrations/`](migrations/).

### 3. Configure

Copy the example config and fill in your secrets:

```bash
cp config.example.yaml config.yaml
```

At minimum set:

```yaml
openai:
  api_key: ${OPENAI_API_KEY}

jwt:
  secret: ${JWT_SECRET}
```

Values support `${VAR}` and `${VAR:default}` environment substitution.

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
./bin/blowball serve                                     # reads ./config.yaml, writes ./data, ./logs, reads ./skills
./bin/blowball serve -d /var/lib/blowball -f /etc/bb/config.yaml   # gather all runtime state under one root
```

The runtime root holds three subdirectories: `data/` (per-user workspaces, session files, per-user skills), `logs/` (the rotated structured log), and `skills/` (global skills). With the default `-d .` this is `./data`, `./logs`, `./skills` — backward compatible with the historical layout, only `./logs` is new. Missing directories are created on startup.

### 6. Run the frontend (optional)

```bash
cd frontend
npm install
npm run dev
```

The Vite dev server starts on port `5173` and proxies `/api` to `http://localhost:8080`.

## Development

```bash
# Build the unified blowball binary (serve + seed subcommands)
make build

# Run all tests with race detection
make test

# Run a single package's tests
go test ./internal/agent/...

# Run a single test
go test ./internal/agent/ -run TestConfuciusDispatchesSubAgent

# Run integration tests (fakes for MySQL/LLM, real orchestrator + handlers)
go test ./test/integration/...

# Static analysis
make lint

# Clean build artifacts
make clean

# Frontend (from frontend/)
npm install
npm run dev
npm run build
npm run lint
npm run generate-api
```

See [`CLAUDE.md`](CLAUDE.md) for the full architecture, conventions, and tool configuration details.

## Project layout

```
.
├── api/openapi.yaml          # OpenAPI 3 spec
├── cmd/
│   ├── seed/                 # CLI to create users
│   └── server/               # HTTP server entry point
├── frontend/                 # React 19 + Vite + TypeScript app
├── internal/
│   ├── agent/                # Agents, orchestrator, OpenAI client
│   ├── config/               # YAML config loader
│   ├── handler/              # HTTP handlers and router
│   ├── middleware/           # Trace, CORS, auth middleware
│   ├── model/                # Domain models
│   ├── pkg/logger/           # Zap logger setup
│   ├── service/              # Business logic layer
│   ├── store/                # MySQL, Redis, filesystem stores
│   ├── stream/               # SSE event stream types and hub
│   └── tool/                 # Tool registry and tool implementations
├── migrations/               # SQL schema migrations
├── skills/                   # Global skills directory
├── test/integration/         # Integration tests
├── config.example.yaml       # Example configuration
├── docker-compose.yaml       # MySQL + Redis
├── Makefile                  # Common tasks
└── CLAUDE.md                 # Detailed developer guide
```

## API overview

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/auth/login` | Exchange credentials for a JWT |
| `GET`  | `/api/v1/sessions` | List sessions |
| `POST` | `/api/v1/sessions` | Create a session |
| `GET`  | `/api/v1/sessions/{session_id}/messages` | Paginated message history |
| `POST` | `/api/v1/sessions/{session_id}/messages` | Send a message, stream SSE |
| `DELETE` | `/api/v1/sessions/{session_id}` | Delete and archive a session |
| `GET`  | `/api/v1/workspace/files` | List workspace files |
| `POST` | `/api/v1/workspace/upload` | Upload a file |
| `GET`  | `/api/v1/workspace/files/{path}` | Download a file |
| `GET`  | `/api/v1/workspace/files/{path}/content` | Read file text content |
| `GET`  | `/api/v1/workspace/files/download/{path}?token=<jwt>` | Token-authenticated download |
| `DELETE` | `/api/v1/workspace/files/{path}` | Delete a file or directory |
| `GET`  | `/api/v1/mcp/tools` | List available tools |
| `GET`  | `/api/v1/skills` | List available skills |
| `GET`  | `/healthz` | Health check |

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

### Security considerations

- **Allowlist only** — only servers declared in `mcp.servers` are connected, and only tools explicitly allowed per agent are exposed to that agent.
- **Auth injection** — use `headers` (SSE / HTTP) and `env` (stdio) for secrets; both support environment substitution so credentials never need to be hard-coded in config.
- **Timeouts** — per-server `timeout` and `call_timeout` prevent a slow or hung remote server from blocking agent turns indefinitely.
- **Subprocess / network sandboxing** — stdio subprocesses run with normal OS process boundaries; additional sandboxing (e.g. seccomp, Landlock, or chroot) is future work.
- **Remote errors** — failures from an MCP server are surfaced as `tool_error` / `agent_error` stream events and do not crash the agent loop.

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

Blowball discovers skills from two locations:

- Global skills: `./skills/`
- Per-user skills: `data/{userID}/skills/`

User skills override global skills of the same name.

### Enabling skills for an agent

```yaml
agents:
  confucius:
    skills:
      - coding-style
      - review-checklist
```

When an agent has skills configured, the skill catalogue is injected into its system prompt. Agents can load skill instructions on demand via:

- `luban_read_skill(name)` — recommended for new configs.
- `read_skill(name)` — legacy tool kept for backward compatibility.

The `luban_*` tools (`luban_list_skills`, `luban_read_skill`, `luban_install_skill`) are registered only when an agent explicitly lists them in its `tools`.

### Listing skills

`GET /api/v1/skills` returns the skills visible to the authenticated user:

```json
{
  "skills": [
    {"name": "coding-style", "filename": "coding-style", "size": 1234, "update_time": "2026-06-16T..."}
  ]
}
```

### Migrating from flat skill files

Previous versions stored per-user skills as flat files such as `data/{userID}/skills/coding-style.md`. To migrate:

1. Create a directory for each skill: `data/{userID}/skills/{skill-name}/`.
2. Move the file into the directory as `SKILL.md`.
3. Add YAML frontmatter with `name` and `description`.

Example:

```bash
mkdir -p data/u-123/coding-style
mv data/u-123/coding-style.md data/u-123/coding-style/SKILL.md
# add ---/name/description/--- frontmatter to SKILL.md
```

## Sandboxed command execution

On Linux, Blowball can register `bash` and `python` tools that run commands inside a [bubblewrap](https://github.com/containers/bubblewrap) sandbox. Each invocation gets its own user/mount/pid/network namespaces, the user's workspace is bound to `/workspace`, and the environment is restricted to an allow-list.

```yaml
tools:
  executor:
    bash:
      enabled: true
      timeout: 30s
      max_output_bytes: 65536
      allowed_env_patterns: ["PATH", "HOME", "LANG", "USER", "TERM"]
      network: false
    python:
      enabled: true
      timeout: 30s
      max_output_bytes: 65536
      allowed_env_patterns: ["PATH", "HOME", "LANG", "USER", "TERM", "PYTHON*"]
      network: false
```

- Network access is disabled by default (`network: false` adds `--unshare-net`).
- If a tool is enabled but `bwrap` is not installed, the server exits with a fatal error.
- On macOS and Windows the tools are silently unavailable regardless of config.

## Configuration

Key sections in `config.yaml`:

- `server` — HTTP port.
- `openai` — API key, base URL, and default model.
- `mysql` / `redis` — connection settings.
- `jwt` — signing secret and token expiry (e.g. `7d`).
- `agents` — system prompts, models, max tokens, tool lists, MCP permissions, skill lists, and `thinking` / `reasoning_effort` for each agent.
- `tools` — enable/disable tool families (xizhi, webfetch, executor) and set timeouts.
- `mcp` — external MCP server declarations.
- `logging` — level; `format` (`json`, the default, or `console`); `output` (sinks, default `["stderr", "file"]`); `file.*` rotation settings for the file sink (`max_size_mb`, `max_backups`, `max_age_days`, `compress`, via lumberjack). The file sink writes `{data-dir}/logs/blowball.log`. Set `output: ["stderr"]` to run console-only with no log file.

## License

MIT
