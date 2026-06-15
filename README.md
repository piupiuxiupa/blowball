# blowball

A Go backend for a multi-agent chat workspace. It exposes a JWT-secured HTTP API built with [Gin](https://gin-gonic.com/), persists sessions and messages in MySQL, caches session state in Redis, and orchestrates a small team of LLM agents backed by OpenAI.

## Features

- **JWT authentication** with bcrypt-hashed passwords.
- **Session management** — create sessions, list them, and fetch paginated message history.
- **Server-Sent Events (SSE)** streaming for agent responses with fine-grained event types:
  `agent_start`, `token`, `tool_call`, `agent_end`, `agent_error`, `done`.
- **Multi-agent orchestration** — a central `Confuse` agent dispatches to specialist agents:
  - `Chongzhi` — coding agent with workspace file tools.
  - `Liang` — analysis and explanation agent.
- **Workspace file tools** (`xizhi_*`) scoped per user: read, write, modify, list, tree, glob, plus `webfetch`.
- **External MCP client support** — connect SSE or stdio MCP servers at startup and proxy their tools into the agent tool catalogue.
- **Per-user workspace isolation** on disk, with best-effort Linux Landlock sandboxing.
- **Graceful shutdown**, structured JSON logging with zap, and OpenAPI 3 spec at [`api/openapi.yaml`](api/openapi.yaml).

## Quick start

### 1. Requirements

- Go 1.26+
- MySQL 8.0
- Redis 7
- An OpenAI API key

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

The API has no public sign-up endpoint. Use the seed CLI:

```bash
make build
./bin/seed -username alice
```

You will be prompted for a password. The tool stores a bcrypt hash and prints the generated `user_id`.

### 5. Run the server

```bash
make run
```

The server listens on the port configured in `config.yaml` (default `8080`).

## Development

```bash
# Build server + seed
make build

# Run all tests with race detection
make test

# Static analysis
make lint

# Clean build artifacts
make clean
```

## Project layout

```
.
├── api/openapi.yaml          # OpenAPI 3 spec
├── cmd/
│   ├── seed/                 # CLI to create users
│   └── server/               # HTTP server entry point
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
├── test/integration/         # Integration tests
├── config.example.yaml       # Example configuration
├── docker-compose.yaml       # MySQL + Redis
└── Makefile                  # Common tasks
```

## API overview

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/auth/login` | Exchange credentials for a JWT |
| `GET`  | `/api/v1/sessions` | List sessions |
| `POST` | `/api/v1/sessions` | Create a session |
| `GET`  | `/api/v1/sessions/{session_id}/messages` | Paginated message history |
| `POST` | `/api/v1/sessions/{session_id}/messages` | Send a message, stream SSE |
| `GET`  | `/api/v1/workspace/files` | List workspace files |
| `POST` | `/api/v1/workspace/upload` | Upload a file |
| `GET`  | `/api/v1/workspace/files/{path}` | Download a file |
| `GET`  | `/api/v1/workspace/files/{path}/content` | Read file text content |
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
```

Supported transports:

- `sse` — connects over Server-Sent Events + HTTP POST messages.
- `stdio` — spawns a local subprocess and speaks JSON-RPC over stdin/stdout.

Configuration fields:

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Unique server identifier. |
| `transport` | yes | `sse` or `stdio`. |
| `url` | for `sse` | Server SSE endpoint. |
| `command` | for `stdio` | Executable to spawn. |
| `args` | no | Command-line arguments for `stdio`. |
| `env` | no | Environment variables injected into the `stdio` child process. |
| `headers` | no | HTTP headers sent with every SSE request. |
| `timeout` | no | Connection / initialization timeout (default `30s`). |
| `call_timeout` | no | Per-tool-call timeout (default `30s`). |
| `reconnect` | no | Reconnect (`sse`) or restart (`stdio`) on failure. |
| `prefix` | no | Prefix applied to every discovered tool name to avoid collisions. |

All string values support `${VAR}` and `${VAR:default}` environment substitution.

Tool names must be unique across built-in tools and all configured MCP servers. If a remote tool name collides, startup fails unless you set a `prefix` for that server. Discovered tools are cached at startup and exposed through `GET /api/v1/mcp/tools`; agents can include them in their configured `tools` list.

### Security considerations

- **Allowlist only** — only servers declared in `mcp.servers` are connected. There is no runtime discovery or dynamic registration.
- **Auth injection** — use `headers` (SSE) and `env` (stdio) for secrets; both support environment substitution so credentials never need to be hard-coded in config.
- **Timeouts** — per-server `timeout` and `call_timeout` prevent a slow or hung remote server from blocking agent turns indefinitely.
- **Subprocess / network sandboxing** — stdio subprocesses run with normal OS process boundaries; additional sandboxing (e.g. seccomp, Landlock, or chroot) is future work.
- **Remote errors** — failures from an MCP server are surfaced as `tool_error` / `agent_error` stream events and do not crash the agent loop.

## Configuration

Key sections in `config.yaml`:

- `server` — HTTP port.
- `openai` — API key, base URL, and default model.
- `mysql` / `redis` — connection settings.
- `jwt` — signing secret and token expiry (e.g. `7d`).
- `agents` — system prompts, models, max tokens, and tool lists for each agent.
- `tools` — enable/disable tool families and set timeouts.
- `logging` — level and format (`json` or `console`).

## License

MIT
