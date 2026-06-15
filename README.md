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
