# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Blowball is a Go backend for a multi-agent chat workspace: a JWT-secured HTTP API (Gin), sessions/messages persisted directly to MySQL (Redis is a read cache, not a durability layer), and OpenAI-compatible agent orchestration. The React frontend lives in the separate `blowball-frontend` repo.

Prefer these sources over re-deriving from code:

- `AGENTS.md` — contributor guide: package layout, style, Conventional Commits (commit after each completed feature), testing rules.
- `api/openapi.yaml` — the authoritative HTTP contract (routes, schemas, error codes). When it changes, copy it into `blowball-frontend/` and run `npm run generate-api` there.
- `openspec/specs/<capability>/spec.md` — authoritative behavior spec per capability (~60 specs: context-compaction, dynamic-subagents, turn-artifacts, scheduled-task-service, …). `openspec/changes/` holds in-flight proposals; keep specs synced when behavior changes.
- `docs/` — runbooks: `scheduled-tasks.md` (scheduler deploy/migrate/rollback + internal RPC security), `shared-storage.md`, `redis-first-persistence.md` (historical).

## Commands

```bash
make build                # ./bin/blowball — dev monolith (`serve` --role all|api|agent + `seed`)
make run                  # monolith serve (--role all: full route set on server.port, default 8080)
make build-api            # ./bin/blowball-api — standalone api-role service (also carries `seed`)
make build-agent          # ./bin/blowball-agent — standalone agent-role service
make run-api              # blowball-api serve — CRUD partition on server.port
make run-agent            # blowball-agent serve — streaming partition on server.agent_port (default 8081)
make build-scheduler      # ./bin/blowball-scheduler — the standalone Scheduler (scheduling is never embedded)
make run-scheduler        # run it with services/scheduler/config.yaml
make test                 # go test -race ./...
go test ./pkg/agent/ -run TestName    # single test
go test ./test/integration/...             # integration (fake MySQL/Redis/LLM, real handlers+orchestrator)
make lint                 # go vet ./...
make proto                # regenerate api/gen/go from api/proto (needs protoc)
./bin/blowball seed --username alice [--password pw] [--dry-run]   # create a user (blowball-api seed is identical)

docker compose -f deploy/compose/mysql-redis.yaml up -d   # MySQL + Redis only; migrations/ auto-run on a FRESH volume only
docker compose -f deploy/compose/all-in-one.yaml up -d --build   # full stack: apiserver+agent+scheduler+officevers+mysql+redis+minio
cp config.example.yaml config.yaml    # then set openai.api_key, jwt.secret, mysql/redis creds
```

Runtime root: `-d/--data-dir` (default `.`) holds `data/ logs/ skills/ tools/ skills-market/ mcp-market/`; config via `-f/--config` (default `config.yaml`). All config strings expand `${VAR}` / `${VAR:default}`.

The `bash` executor tool is Linux-only (bubblewrap/`bwrap` + unprivileged user namespaces; skipped on macOS/Windows, `enabled: false` by default).

## Architecture

### Monorepo layout

One Go module (`github.com/lush/blowball`) builds four services, each with its own directory, thin `cmd/` entry, and private `internal/`:

- `services/apiserver/` → `blowball-api` (api role pinned; exports the `apiserver` assembly package + `seed`).
- `services/agent/` → `blowball-agent` (agent role pinned; exports the `agentserver` assembly package whose `Assembly` owns the ordered shutdown drain).
- `services/scheduler/` → `blowball-scheduler`.
- `services/officevers/` → office 文件版本服务, merged from the former standalone `office-vers` repo (specs `version-rollback`/`versioned-file-storage` are in the root openspec).

All cross-service/cross-role code lives in root `pkg/` (the pre-split `internal/` tree moved wholesale — package-level role exclusivity does not exist; the api/agent split is at wiring level: `apiserver.Wire` vs `agentserver.Wire` over the shared `pkg/boot.Runtime`). `cmd/blowball` at the repo root stays as the dev monolith (`serve --role all|api|agent` + `seed`), composing the same assembly packages — behavior identical to the standalone binaries per role. `migrations/` and `api/` remain at the root (shared DB schema and cross-service contracts). Deployment artifacts live in `deploy/<svc>/` (per-service Dockerfile + Jenkinsfile, scheduler systemd unit) and `deploy/compose/` (`mysql-redis.yaml` base, `all-in-one.yaml` full stack with the shared `blowball_data` volume across apiserver/agent).

### Process roles and the Scheduler split

- `blowball serve --role all|api|agent` (default `all` = the dev monolith) and the standalone binaries `blowball-api` / `blowball-agent` (role pinned, no `--role` flag, same routes/ports/config as the monolith's corresponding role). The `api` role registers CRUD/history/scheduled-task-management routes and never constructs an orchestrator, OpenAI client, or tool registry (a missing `openai.api_key` is only a warning). The `agent` role registers the streaming message endpoint, turn lifecycle, and `/api/v1/mcp/tools`, and owns the full turn pipeline. Route partitioning lives in `pkg/handler/router.go` (`RegisterAPIRoutes` / `RegisterAgentRoutes` / `RegisterRoutes`). All roles share the same `-d` data plane (MySQL/Redis/local FS) — in split deployments apiserver and agent containers must mount the same data volume.
- Scheduling is a separate binary: `services/scheduler/` owns `scheduled_tasks`/`scheduled_task_runs` (materialization, claims, leases) and never constructs an Agent or loads an LLM key. The main service keeps the turn engine (`pkg/turn`), task conversations, `.cron` resource versions (`pkg/resource`), and the execution Runtime `pkg/scheduledruntime` (idempotent `SubmitScheduledRun` receipts, CAS/lease prep, start grants, watchdog, terminal facts, conservative reconciliation — unknown ⇒ `uncertain`, never replayed).
- They talk over authenticated internal gRPC: contracts in `api/proto/{scheduler,scheduledruntime}/v1`, generated code in `api/gen/go`, shared error codes / mTLS / service-identity authz in `pkg/rpcx`. Service identities are certificate CNs: `blowball-scheduler` and `blowball-main`/`blowball-api`/`blowball-agent`.
- `internal_rpc:` configures that link and is OFF by default: task HTTP endpoints then return 503 (no local fallback), scheduled-task agent tools are not registered, and `.cron` writes fail closed. `security_mode: mtls` (default) or explicit `plaintext_token` (loopback dev only; no anonymous mode, no TLS-error fallback).
- `scheduled_tasks.enabled: true` fails config load — the embedded scheduler was removed. See `docs/scheduled-tasks.md` for deploy/migrate/rollback.
- Task sessions live in the physically separate `scheduled_*` table group (`pkg/store/mysql` `Tables`/`SessionStore`, `ForSource`), read-only through `/api/v1/scheduled-sessions...`; ordinary endpoints 404 on scheduled IDs and vice versa.

### Turn pipeline

`MessageStreamHandler.SendMessage` (`pkg/handler/message_stream.go`) → ownership + attachment validation → admission (Redis session-run claim; 409 `SESSION_BUSY`, or FIFO queue admission when `messages.queue.enabled`) → `startTurn` (`pkg/handler/turn_start.go`, shared by the HTTP fast path, the queue pump, and scheduled tasks via `pkg/turn.Executor`): preamble history recovery / compaction / memory recall → **send-time user-row persist** (must stay AFTER every history read of the turn, else the message enters recovered history AND the LLM context twice) → orchestrator runs on a turn-scoped context held by `pkg/run` → events mirror into the Redis Stream `run:{rid}:events` → the SSE response and the resume endpoint (`GET .../turns/:run_id/events`; SSE `id:` = stream entry id, so `Last-Event-ID` resume is exact) are the same subscription loop. **Client disconnect does not cancel the turn** — every terminal path (success/failure/cancel) persists through the round-final batch path on a detached context. A status-running run whose heartbeat expired is dead: cancel force-clears it to `interrupted`. With the queue enabled, the turn goroutine pumps the queue head into the next turn on the same run id/stream.

### Persistence

MySQL is authoritative; Redis `msgs:{sid}` is a read cache with best-effort appends (the write-behind ingest queue is retired).

- **The round is the persistence unit**: one completed LLM reply + its tool executions land in ONE transaction (`SessionService.SaveFinalMessages` → `AppendFinalBatch`). `messages` stores final rows (user/assistant/tool-placeholder/agent-error); `tool_calls` is the canonical tool-interaction store — one row pairs call+result keyed by `tool_call_id`, UK `(session_id, tool_call_id)` makes redelivery idempotent.
- Deterministic `client_msg_id = {trace_id}:{msg_index}` (user row `:0`, round finals `:1..N`) lets the send-time write, the incremental round persister, the mid-turn compaction flush, and the terminal batch collapse on overlap.
- Reconstruction (`pkg/turn/message_reconstruct.go`, shared with the handler layer via `pkg/handler/turn_compat.go`) rebuilds wire-format history and joins tool placeholders to `tool_calls` payloads. Except for explicit compaction rewrites, the already-sent prefix must stay **byte-identical** turn-over-turn (provider prefix-cache stability). The display/history path never sees compaction — no summary rows, no gaps.
- Migrations are sequential (`migrations/NNN_description.sql`); the initdb mount runs them only on a fresh volume — existing databases apply new ones manually.
- Other main tables: `turn_usage` (the `done` event's usage object; `context_tokens` = last-round size, what turn-start compaction checks), `llm_raw_log` (raw LLM request/response capture; deliberately no FK to sessions and no cleanup job), `context_compactions`, `subagent_instances`/`subagent_runs`, `attachments`. Session deletion is a single-transaction archive+purge into `*_deleted` mirrors.

### Agent layer (`pkg/agent/`)

- `Agent.Run(ctx, messages, hub)` + `LLMClient.StreamChat`. The `Orchestrator` builds a fresh graph per request; the root `Confucius` runs a tool-calling loop with parallel tool dispatch and a synthetic root-only `update_plan` tool.
- **Dynamic sub-agents** (`agents.subagent`): all spawns go through the synthetic `spawn_subagent` tool; each dispatch builds a fresh generic `SubAgent` from a spec (task prompt, subset-only narrowed tool set, round cap) and discards it after the call. Tree budgets: `max_depth` (default 1 = flat), `max_concurrent`, `max_total_per_turn`. The dispatch layer stamps every sub-agent event with `Meta.agent_instance_id` (stable thread id) + `Meta.parent_tool_call_id` (per-dispatch run id) — the identity keys for SSE, persistence, and frontend grouping.
- **Model catalog**: `openai.models` (mandatory) is the single source of per-turn model/effort/output-quota — resolved from request params ‖ catalog defaults in `pkg/modelselect`, injected as one `ModelOverride` into every agent of the turn. Agents carry NO model/thinking/quota fields; residual `agents.<name>.model|thinking|reasoning_effort|max_tokens`, `openai.model`, and the global `openai.length_continue` fail config load with migration-pointing errors. Entry fields: `name, max_context_tokens, max_completion_tokens, thinking, vision?, length_continue?`.
- **Per-user LLM tokens**: `agent.ClientResolver` resolves the gateway credential per call from ctx `user_id` (`user_llm_credentials`, managed via `/api/v1/me/llm-token`). Unconfigured → global key; configured-but-invalid fails explicitly, never falls back. The token never appears in responses (masked suffix only), logs, or raw capture.
- Round-cap hits run one tool-disabled wrap-up round (synthesize an answer; `agent_error(round_cap_exhausted)` + `done.error` if that yields nothing). One `agents.<name>.retry` block governs round-level (each `StreamChat`) and dispatch-level (sub-agent Run) transient retries against a turn-wide token budget; cancellation is never retried. `finish_reason=length` rounds continue with an expanded budget when the entry configures `length_continue`. `openai.stream_idle_timeout` (opt-in) bounds inter-frame gaps on streaming calls.
- **Context compaction** (`pkg/service/compaction.go`): at 80% of the input budget (`max_context_tokens − max_completion_tokens − 4096`), measured by pre-request estimation (never provider usage alone), the middle history is summarized into a merging checkpoint (first message anchored, tail retained against the budget, tool call/result pairs never split). Compaction is silent, never blocks a turn, and is the only thing allowed to rewrite already-sent context.
- **Cross-session memory** (`pkg/memory/`, default off, external OpenViking): turn-start recall is injected as a persisted-and-replayed synthetic `recall_memories` tool pair; turn-end capture is a fire-and-forget side effect. All failures WARN + degrade — memory never fails a turn.
- **Turn artifacts** (`pkg/artifact/`): markdown links `[..](blowball://workspace/<rel>)` in the final reply are the sole attribution signal (never workspace discovery/mtimes); snapshots go to the office-vers service + `file_versions` index, announced as `artifact` SSE events before `done`.
- **Image attachments**: magic-byte-sniffed uploads (png/jpeg/webp/gif, ≤8 MiB, session-scoped or staged) inline into the CURRENT turn only — history renders deterministic placeholders — and require a `vision: true` catalog entry (400 `MODEL_NOT_VISION` otherwise).

### Tools, MCP, skills

Process-wide `tool.Registry` (`pkg/tool/registry.go`) resolves configured tool names and renders `tools[]`; per-dispatch narrowing builds scoped child registries. `tools.timeouts` bounds per-tool execution centrally at the registry dispatch entry point. Every registry-tool result is rendered into a uniform envelope `{"status":0,"result":...}` / `{"status":1,"error":...}` — tool failures surface through the envelope, not `agent_error` events.

- `pkg/tool/xizhi/` — per-user workspace file tools scoped to `data/{userID}/workspace`; `validatePath` rejects absolute/`..`/symlink escapes, and the `.blowball/` namespace is banned from agent file tools (it holds per-user MCP configs). `xizhi_grep`/`xizhi_find` shell out to `rg`/`fd` when present, else pure-Go fallbacks with identical results.
- `pkg/tool/webfetch/` — bounded fetch with threshold-gated model digestion on the turn's per-user client.
- `pkg/tool/executor/` — `bash` in a bubblewrap sandbox: fresh user/mount/pid namespaces, workspace bound at `/workspace`, env built in three tiers (host allowlist < operator `bash.env` literals < forced HOME/PATH/PYTHONPATH invariants), operator CLIs from `{data-dir}/tools` on `$HOME/.local/bin`. `.cron` mounts read-only.
- `pkg/tool/luban/` + `pkg/tool/skill/` — skill tools and the shared loader; name resolution is `user > global > market`.
- MCP: operator servers (`mcp.servers` — startup-connected proxies registered in the base registry) AND per-user servers (`pkg/tool/mcp` — turn-scoped, workspace-resident `.blowball/mcp/{name}/config.json`, HTTP transport + static credentials only, auth injected server-side and redacted from all model-visible I/O, oversized results spill to `tmp/mcp-outputs/`). `mcp_market:` adds a remote-authorized second source (`workspace > market`).
- Skill market (`skill_market:`): remote JWT-authed allowlist + operator disk sync at `{data-dir}/skills-market/`. Fail-closed on every failure shape — never a directory-scan fallback (that would expose other users' skills).

Agent tool visibility is strictly configured (`agents.<name>.tools` / `.mcp.servers` / `.skills`); `spawn_subagent` and `update_plan` are synthetic and never in the registry.

### SSE

Event types (`pkg/stream/event.go`): `agent_start`, `token`, `reasoning`, `tool_call`, `tool_result`, `agent_end`, `agent_error`, `artifact`, `done`. `done` carries the authoritative usage object `{total, by_agent, meta}` (durability home: `turn_usage`). The hub decouples producers from the HTTP writer; slow clients never block agent loops.

### Storage backend & sandboxing

`storage.workspace.backend: local|shared` — `shared` boots only on a writable FUSE-family mount (JuiceFS with `--allow-other`) enforced by a startup health check; all POSIX file ops are unchanged. Landlock scopes the process to the runtime subdirs with operator-configurable baselines (`landlock:`, `tools.executor.sandbox:`); non-Linux dev skips it.

## Conventions that catch mistakes

- Removed config keys fail config load (or the registry's unknown-tools startup check) with migration-pointing errors — e.g. the legacy agent model fields, `openai.model`, global `openai.length_continue`, `agents.chongzhi`/`agents.liang`, `xizhi_glob_files`, `scheduled_tasks.enabled: true`, `messages.flush_interval`/`flush_batch_size` (write-behind flusher). When retiring a key, keep that pattern.
- MySQL and filesystem store methods return `(nil, nil)` on missing records, not errors.
- Turn-path logging goes through `logger.FromContext(ctx)` (session_id + trace_id attached); `Registry.Call` emits the central tool-dispatch log line for every registry-tool call.
- Gin wildcard trap: workspace file GET/PUT/POST share one catch-all `/workspace/files/*path` with internal suffix dispatch (`download`, `/content`, `onlyoffice-config`); `/workspace/search` and `/workspace/upload` must fork statically before the wildcard — gin rejects a static segment beside a wildcard at one node.
- Security invariants to preserve when touching these paths: per-user workspace path scoping, the `.blowball` tool-path ban, response masking + log/capture exclusion for per-user LLM tokens and MCP credentials, JWT auth everywhere except `/healthz` and `/auth/login`.
- There is no public user-creation endpoint — users come from `blowball seed`. `auth.password_required: false` enables passwordless login for seeded users (dev).
- Never commit `config.yaml`, credentials, runtime data, logs, or `bin/` artifacts.
- Keep `CLAUDE.md`, `AGENTS.md`, and the relevant `openspec` specs in sync when behavior changes.
