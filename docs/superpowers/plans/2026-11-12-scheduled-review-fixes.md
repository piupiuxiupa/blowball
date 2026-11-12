# Scheduled Review Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Each task has its own red/green test cycle and scoped commit.

**Goal:** Fix the seven confirmed regressions in the standalone Scheduler change without expanding its public contract.

**Architecture:** Bind the shared turn executor to scheduled persistence explicitly. Persist an execution hold before releasing an occurrence's active state; preserve active state when hold persistence fails. Correct the remaining SQL, schema, file-tool operation and one-shot scheduling mismatches in their owning layers.

**Tech Stack:** Go 1.26-compatible code, Gin, MySQL, Redis, standard testing and existing Testify fakes.

**Spec:** `openspec/changes/extract-scheduler-service/design.md` and its scheduled-task-service, scheduled-task-sessions, scheduled-agent-tasks and resource delta specs.

## Global Constraints

- Keep Scheduler standalone; do not introduce direct cross-service SQL ownership or new RPC/HTTP contracts.
- Keep scheduled and interactive persistence separate, including title, compaction and sub-agent snapshots.
- Keep runtime guidance and resource context out of canonical user text, MySQL, titles and memory capture.
- Do not replay an uncertain execution or an already consumed `at` slot.
- Preserve owner checks, draft fences, protected published resources and ordinary workspace behavior.
- Use an incremental migration for databases that already applied migration 028.
- Use a dedicated test database for MySQL tests; never run destructive fixtures against application data.
- Format changed Go code and commit only each task's source/tests/docs after relevant tests pass.

## Review Focus

- Missing scheduled dependencies must remain fail-closed; production wiring must supply the complete table group.
- A failed hold write must not terminalize the active occurrence; a later reconciliation must be able to retry.
- Startup rejection must preserve both its recognized terminal status and its actual reason.
- Scheduled placeholder pagination and sub-agent history must read back on the real migrated schema.
- A paused or initially disabled future `at` task must retain its unconsumed slot, while consumed `at` and paused cron behavior remain unchanged.

## Execution

The user approved the seven fixes in the preceding review. Use the current clean checkout on `codex/fix-scheduled-review-regressions`; keep integration-sensitive wiring and hold ordering local. Bounded, file-disjoint fixes may run in parallel. The OpenSpec task list is the durable completion ledger.

### Task 1: Wire scheduled turn persistence

**Files:** `cmd/blowball/serve.go`, its focused tests, `internal/handler/message_stream.go`, its focused tests.
**Interface:** `newScheduledTurnServices(rt *appRuntime, llmClient agent.LLMClient, titleCfg config.OpenAIConfig, defaultMaxContext int) *turn.ScheduledServices`; allow the stream-handler constructor to receive optional scheduled services without changing existing callers.
- [ ] Add regressions for a complete scheduled service bundle and a scheduled turn reaching source-bound services through the stream handler.
- [ ] Observe the missing wiring failure, retaining the existing no-services fail-closed test.
- [ ] Construct the scheduled MySQL view, sessions/messages/titles/compaction/snapshots and pass it to `Dependencies.Scheduled`.
- [ ] Run `go test ./cmd/blowball ./internal/handler ./internal/turn`.
- [ ] Commit `fix(turn): wire scheduled persistence into production executor`.

### Task 2: Preserve the hold safety boundary

**Files:** `services/scheduler/internal/scheduler/scheduler.go`, `scheduler_test.go`.
**Interface:** Keep existing store methods; call `SetHold` before `MarkInterruptedFromReceipt`, returning before projection if the hold write fails.
- [ ] Add tests observing hold presence before interrupted projection and injecting a failed hold write followed by a successful retry.
- [ ] Observe the unsafe ordering/terminalization failures.
- [ ] Reorder the two operations without changing grants, stop-proof clearing or RPC interfaces.
- [ ] Run `go test -race ./services/scheduler/internal/scheduler`.
- [ ] Commit `fix(scheduler): persist execution hold before interrupted projection`.

### Task 3: Persist startup rejection status and reason

**Files:** `internal/store/mysql/scheduled_receipt.go`, `scheduled_receipt_test.go`.
**Interface:** Preserve `FinishScheduledReceiptBeforeStart`; bind its `status` and `reason` to separate SQL placeholders.
- [ ] Add a real-store rejection/readback test asserting terminal classification, reason and not-started proof.
- [ ] Observe the stored `terminal`/incorrect reason failure.
- [ ] Correct only the update statement and argument order.
- [ ] Run the receipt tests with `MYSQL_TEST_DSN` bound to the dedicated test database.
- [ ] Commit `fix(runtime): persist recognized pre-start receipt terminal facts`.

### Task 4: Use a supported file-write operation kind

**Files:** `internal/tool/xizhi/register.go`, `guard_test.go`.
**Interface:** The write-file tool must pass a guard-supported create/modify kind for write and append modes.
- [ ] Add a caller/guard contract regression for valid owned draft writes, preserving denied managed paths and ordinary writes.
- [ ] Observe the unsupported `write` operation failure.
- [ ] Use a supported operation kind; keep ownership/fence checks before file mutation.
- [ ] Run `go test ./internal/tool/xizhi ./internal/resource`.
- [ ] Commit `fix(xizhi): authorize draft writes with supported guard operation`.

### Task 5: Fix scheduled placeholder pagination

**Files:** `internal/store/mysql/message.go`, `message_test.go`.
**Interface:** Both placeholder page helpers and `subagentPlaceholderFilter` must use the same fixed inner-table alias for either physical table group.
- [ ] Add scheduled placeholder ascending/descending readback regressions, including an empty page and full-view/ordinary controls.
- [ ] Observe the unknown `messages.content` qualifier failure on MySQL.
- [ ] Give both inner page sources the fixed alias required by the filter.
- [ ] Run the focused message tests on the dedicated MySQL test database.
- [ ] Commit `fix(mysql): alias scheduled placeholder pagination sources`.

### Task 6: Complete scheduled sub-agent run timestamps

**Files:** New `migrations/031_scheduled_subagent_run_timestamps.sql`, `internal/store/mysql/scheduled_store_test.go`, `subagent_run_test.go`, `docs/scheduled-tasks.md`.
**Interface:** Add `create_time` and `update_time` matching ordinary sub-agent runs; apply the new migration in scheduled fixtures.
- [ ] Add scheduled list/get/child/history readback regressions after writing a run.
- [ ] Observe the missing timestamp-column failures against the original schema.
- [ ] Add the forward migration and backfill existing run timestamps from started/finished times; document manual application.
- [ ] Verify migrated fresh tables and pre-migration rows on the dedicated MySQL test database.
- [ ] Commit `fix(mysql): migrate scheduled subagent run timestamps`.

### Task 7: Preserve future one-shot slots across pause

**Files:** `services/scheduler/internal/task/service.go`, `service_test.go`.
**Interface:** A future unconsumed `at` keeps `NextRunAt` while disabled; cron resume recomputes its next slot; consumed `at` remains nil.
- [ ] Add future-at pause/resume and initially-disabled-at enable regressions.
- [ ] Observe `enabled=true` with a lost `NextRunAt`.
- [ ] Clear the cursor on disable only where the existing cron resume path restores it.
- [ ] Run `go test ./services/scheduler/internal/task`.
- [ ] Commit `fix(scheduler): retain unconsumed at slots while paused`.

## Final Verification

- [ ] Recheck the complete diff and all seven scoped commits; synchronize OpenSpec scenarios/task checkboxes.
- [ ] Run focused MySQL regressions without skips, `make test`, `make lint`, both binary builds to temporary output paths and OpenSpec strict validation.
- [ ] Confirm the worktree is clean and no credentials, binaries, test data or temporary probes are committed.
