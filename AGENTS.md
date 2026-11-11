# Repository Guidelines

## Project Structure & Module Organization

Blowball is a Go backend for a multi-agent chat workspace. The executable CLI lives in `cmd/blowball/`; keep entrypoint wiring there and place reusable logic under `internal/`. Major backend layers include `internal/agent` (orchestration and OpenAI integration), `internal/turn` (transport-independent full-turn execution shared by HTTP, queue continuation, and scheduled tasks), `services/scheduler/` (the standalone Scheduler binary: task CRUD, materialization, claims, start authorization, reconciliation), `internal/scheduledruntime/` (the agent/all execution Runtime: receipts, start grants, watchdog, terminal facts), `internal/resource/` (the `.cron` draft/publication/guard service), `internal/rpcx/` (shared gRPC error codes, mTLS and service-identity middleware), `internal/schedule` (pure at/cron rules), `internal/modelselect` (neutral model-catalog resolution), `handler` (Gin routes), `service` (business logic), `model`, `store` (MySQL, Redis, and filesystem persistence), `middleware`, `config`, `run`, `stream`, and `tool`. Internal gRPC contracts live in `api/proto/` with generated code in `api/gen/go/` (regenerate with `make proto`). Cross-cutting helpers belong in focused `internal/pkg/` packages. HTTP contracts are documented in `api/openapi.yaml`; SQL migrations use sequential names in `migrations/`. Unit tests are colocated with packages, while end-to-end scenarios live in `test/integration/`. Deployment material is under `deploy/` and `scripts/`; scheduled-task rollout/rollback is documented in `docs/scheduled-tasks.md`.

OpenSpec change proposals live under `openspec/changes/`. Keep each change's proposal, design, delta specs, and task list synchronized with the implemented behavior; update `api/openapi.yaml` when implementation work introduces or changes a client-visible HTTP contract.

## Runtime Context Conventions

Keep durable canonical history (MySQL and `msgs:{session_id}`) separate from the active model transcript. Root `<current_status>` and root/SubAgent `<tool_budget>` messages are cache-only synthetic user messages: they never enter MySQL, SubAgent transcripts, SSE, titles, or real user ordinals. The root status log lives in `runtime_status:{session_id}` (with its id registry), is immutable and append-only, and replays only when every before-user/after-tool anchor is valid. A Redis miss or invalid anchor starts a fresh cache epoch; successful compaction clears old status and builds a fresh status on the new projection, while a stable `ContextCompacted` checkpoint does not clear or reset status. Replayed messages carry the real `RuntimeStatusSeq`; continue from the log's maximum sequence, never from its entry count. Candidate planning reserves one conservative fresh status in `RequestOverheadTokens` before accepting retained tail. Cache failures degrade with WARN logs and never fail a turn.

`max_rounds` counts outer tool-calling rounds, not individual tool invocations. Parallel calls in one response consume one round; retries and length continuations do not increment it. Append one new countdown after each complete tool-result group without mutating older status. `current_status` precedes the real current user message, whose persisted/title/memory content remains the unmodified request text. Context-remaining values are conservative estimates that include system prompt, tools, existing status, and the new status itself.

Compaction Tool Activity keeps the parent `mcp_call` count and nested per-`(server, tool)` call/success/error counts. Merge nested activity deterministically without double-counting children in top-level totals. Legacy or malformed calls stay in an unattributed residual; never invent old server/tool details or drop high-cardinality pairs with a Top-N cap.

## Build, Test, and Development Commands

```bash
docker compose up -d       # Start MySQL and Redis; initializes migrations on a fresh volume
cp config.example.yaml config.yaml
make build                 # Build ./bin/blowball
make build-scheduler       # Build ./bin/scheduler (standalone Scheduler; never embedded)
make run                   # Rebuild and run the server
make test                  # Run all Go tests with race detection
make lint                  # Run go vet ./...
go test ./internal/agent/ -run TestName
go test ./test/integration/...
```

For existing databases, apply new SQL migrations manually; the Docker initdb mount only runs on first initialization.

## Coding Style & Naming Conventions

Use Go 1.26 idioms and standard `gofmt` formatting (tabs, lowercase package/file names). Prefer small, layer-appropriate interfaces and keep HTTP parsing out of services and stores. Name source files `lower_snake.go`, tests `*_test.go`, and migrations `NNN_description.sql`. Keep public identifiers documented when their purpose is not obvious.

## Testing Guidelines

Tests use Go's `testing` package, with Testify and lightweight fakes available. Add focused tests beside the package for bug fixes and new behavior; use `test/integration/` when handler, service, orchestration, and persistence behavior must be exercised together. `make test` must pass before submitting.

## Commit & Pull Request Guidelines

History follows Conventional Commits with imperative, lowercase subjects, for example `feat(agent): ...`, `fix(handler): ...`, or `test(llmraw): ...`. Keep commits scoped to one concern. Pull requests should explain the change and rationale, list tests run, call out API/schema/config migrations, and link related issues. Include logs or screenshots when runtime behavior changes.

Commit immediately after finishing each feature (or self-contained piece of one). Do not leave completed work uncommitted in the working tree; run the relevant tests first, then create a scoped Conventional Commit for the change. Never commit build artifacts such as the `bin/` output of `make build`; commits should contain only source, tests, docs, and config examples.

## Security & Configuration

Never commit `config.yaml`, credentials, runtime data, logs, or build artifacts (e.g. `bin/`). Keep secret examples in `config.example.yaml` and use `${ENV_VAR}` expansion. Preserve JWT checks and per-user workspace path scoping when changing handlers or tools.
