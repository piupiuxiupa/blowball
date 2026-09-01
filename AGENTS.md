# Repository Guidelines

## Project Structure & Module Organization

Blowball is a Go backend for a multi-agent chat workspace. The executable CLI lives in `cmd/blowball/`; keep entrypoint wiring there and place reusable logic under `internal/`. Major backend layers include `internal/agent` (orchestration and OpenAI integration), `handler` (Gin routes), `service` (business logic), `model`, `store` (MySQL, Redis, and filesystem persistence), `middleware`, `config`, `run`, `stream`, and `tool`. Cross-cutting helpers belong in focused `internal/pkg/` packages. HTTP contracts are documented in `api/openapi.yaml`; SQL migrations use sequential names in `migrations/`. Unit tests are colocated with packages, while end-to-end scenarios live in `test/integration/`. Deployment material is under `deploy/` and `scripts/`.

OpenSpec change proposals live under `openspec/changes/`. Keep each change's proposal, design, delta specs, and task list synchronized with the implemented behavior; update `api/openapi.yaml` when implementation work introduces or changes a client-visible HTTP contract.

## Build, Test, and Development Commands

```bash
docker compose up -d       # Start MySQL and Redis; initializes migrations on a fresh volume
cp config.example.yaml config.yaml
make build                 # Build ./bin/blowball
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

## Security & Configuration

Never commit `config.yaml`, credentials, runtime data, or logs. Keep secret examples in `config.example.yaml` and use `${ENV_VAR}` expansion. Preserve JWT checks and per-user workspace path scoping when changing handlers or tools.
