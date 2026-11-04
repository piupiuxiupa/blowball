# Repository Guidelines

Behavioral guidelines to reduce common LLM coding mistakes. Merge with project-specific instructions as needed.

Tradeoff: These guidelines bias toward caution over speed. For trivial tasks, use judgment.

1. Think Before Coding
Don't assume. Don't hide confusion. Surface tradeoffs.

Before implementing:

State your assumptions explicitly. If uncertain, ask.
If multiple interpretations exist, present them - don't pick silently.
If a simpler approach exists, say so. Push back when warranted.
If something is unclear, stop. Name what's confusing. Ask.
2. Simplicity First
Minimum code that solves the problem. Nothing speculative.

No features beyond what was asked.
No abstractions for single-use code.
No "flexibility" or "configurability" that wasn't requested.
No error handling for impossible scenarios.
If you write 200 lines and it could be 50, rewrite it.
Ask yourself: "Would a senior engineer say this is overcomplicated?" If yes, simplify.

3. Surgical Changes
Touch only what you must. Clean up only your own mess.

When editing existing code:

Don't "improve" adjacent code, comments, or formatting.
Don't refactor things that aren't broken.
Match existing style, even if you'd do it differently.
If you notice unrelated dead code, mention it - don't delete it.
When your changes create orphans:

Remove imports/variables/functions that YOUR changes made unused.
Don't remove pre-existing dead code unless asked.
The test: Every changed line should trace directly to the user's request.

4. Goal-Driven Execution
Define success criteria. Loop until verified.

Transform tasks into verifiable goals:

"Add validation" → "Write tests for invalid inputs, then make them pass"
"Fix the bug" → "Write a test that reproduces it, then make it pass"
"Refactor X" → "Ensure tests pass before and after"
For multi-step tasks, state a brief plan:

1. [Step] → verify: [check]
2. [Step] → verify: [check]
3. [Step] → verify: [check]
Strong success criteria let you loop independently. Weak criteria ("make it work") require constant clarification.

These guidelines are working if: fewer unnecessary changes in diffs, fewer rewrites due to overcomplication, and clarifying questions come before implementation rather than after mistakes.

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

Commit immediately after finishing each feature (or self-contained piece of one). Do not leave completed work uncommitted in the working tree; run the relevant tests first, then create a scoped Conventional Commit for the change. Never commit build artifacts such as the `bin/` output of `make build`; commits should contain only source, tests, docs, and config examples.

## Security & Configuration

Never commit `config.yaml`, credentials, runtime data, logs, or build artifacts (e.g. `bin/`). Keep secret examples in `config.example.yaml` and use `${ENV_VAR}` expansion. Preserve JWT checks and per-user workspace path scoping when changing handlers or tools.
