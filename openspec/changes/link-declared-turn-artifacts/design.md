## Context

Turn artifacts are currently detected by walking the caller's entire per-user workspace at turn end and selecting regular files whose mtime is not earlier than the turn's user-message timestamp. Runs are serialized per session, but workspaces are shared per user, so two concurrent sessions can observe each other's newly written files. The system prompt already requires final deliverables to be linked as `[显示名](blowball://workspace/<relative-path>)`; that explicit declaration is a more precise attribution signal.

The orchestrator adapter already accumulates the top-level stream events before emitting `done`. Artifact finalization is invoked at that boundary, so the final user-facing assistant text is available without changing the agent contract.

## Goals / Non-Goals

**Goals:**

- Attribute artifacts only from links in the current turn's top-level assistant content.
- Stop scanning the shared workspace and remove the mtime watermark from artifact finalization.
- Preserve artifact event ordering before `done`, snapshotting, version reuse, ownership checks, and existing read APIs.
- Prevent concurrent sessions from adding each other's deliverables to their artifact summaries.
- Make model compliance consequential and explicit: an unlinked deliverable is omitted from artifacts.

**Non-Goals:**

- Isolating workspaces by session or run.
- Resolving concurrent writes to the same path.
- Detecting unlinked files with filesystem heuristics.
- Changing HTTP endpoints, database schema, or office-vers contracts.

## Decisions

### D1: Final assistant links are the sole artifact declaration

At the `EventDone` boundary, the adapter concatenates `EventToken` content from top-level events only (events without sub-agent run identity). It passes that text to `FinalizeTurn`. Sub-agent transcript content is ignored because it is not the user-facing deliverable declaration.

This makes attribution local to the turn that emitted the reply. A file written by another session cannot enter this turn's artifact list unless this turn explicitly links it.

Alternative rejected: intersecting mtime results with links. It preserves the problematic shared-filesystem assumption and still requires scanning every user file. The product decision is that the link is authoritative.

### D2: Parse only markdown workspace links with the standard library

Artifact finalization recognizes markdown link destinations whose URL scheme is `blowball` and host is `workspace`. It decodes the URL path, rejects absolute paths, traversal, query/fragment components, `tmp/`, and hidden path segments, then deduplicates and sorts the resulting workspace-relative paths.

A small scanner is sufficient because the backend controls and renders the link convention. No Markdown parser or new dependency is needed.

### D3: Keep validation and snapshot behavior at the artifact service boundary

After extracting a path, the service stats the workspace file. Only existing regular, non-empty files are announced and snapshotted; symlinks and directories are excluded. Missing, invalid, empty, and reserved paths are skipped with WARN logs and do not fail the turn.

A linked pre-existing file is a valid artifact. The existing index lookup still determines `create` versus `update`, and byte-identical latest versions still reuse their `version_id`.

### D4: Keep the hook at the adapter's done boundary

`TurnHooks.FinalizeTurn` receives the collected top-level assistant text. The adapter continues invoking it before forwarding `done`, so live clients retain the existing ordering guarantee: zero or more `artifact` events followed by `done.meta.artifacts`.

The message-stream handler no longer captures or passes the user-message timestamp for artifact detection.

## Risks / Trade-offs

- [Model omits a required link] → The deliverable is intentionally absent from artifact records; the prompt states that consequence, and logs/tests make the contract explicit.
- [Malformed or encoded path] → Reject and WARN rather than guessing or exposing workspace-external files.
- [Two sessions link the same path] → Both may emit an artifact; snapshot bytes depend on which write/finalization ordering occurs. This is the existing shared-workspace write-conflict ceiling, not an attribution failure.
- [Artifact history persistence regresses] → Add a regression check that emitted artifact events survive the final message persistence path; this is an existing turn-artifacts requirement.

## Migration Plan

Deploy the backend change with no schema migration. Existing version rows and historical events remain valid. New turns use link-declared artifacts immediately. Rolling back the binary restores mtime scanning without data cleanup.

## Open Questions

None.
