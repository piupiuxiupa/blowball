## 1. Config: per-agent `max_rounds` field + default

- [x] 1.1 In `internal/config/config.go`, add `MaxRounds int \`yaml:"max_rounds"\`` to `AgentConfig` (struct at line 363), with a doc comment stating it bounds the agent's tool-calling loop and that `0`/unset falls back to the default.
- [x] 1.2 In `internal/config/config.go`, add `func DefaultAgentMaxRounds() int { return defaultAgentMaxRounds }` and `const defaultAgentMaxRounds = 100` mirroring the `DefaultRetryMaxAttempts`/`defaultRetryMaxAttempts` pattern (line 358). Default `100` reproduces the current working-tree constants exactly.
- [x] 1.3 In `internal/config/config.go` `validate()`, reject `AgentConfig.MaxRounds < 0` with a clear error (negative is a typo, not "use default"); `0` is valid (means "use default"). Do not coerce to the default at load time — default application happens at agent construction (task 2.2), matching how retry defaults are applied per-agent.

## 2. Agent layer: resolved cap + cap-tracking plumbing

- [x] 2.1 Remove the three constants `maxConfuciusRounds` / `maxChongzhiRounds` / `maxLiangRounds` (currently `100`) from `internal/agent/confucius.go:19`, `chongzhi.go:18`, `liang.go:19`. Add a `maxRounds int` field to each of the three agent structs, set from config at construction.
- [x] 2.2 In `internal/agent/orchestrator.go` `buildConfucius` / `buildChongzhi` / `buildLiang` (lines 114/122/130), resolve the effective cap before constructing each agent: `maxRounds := cfg.MaxRounds; if maxRounds <= 0 { maxRounds = config.DefaultAgentMaxRounds() }`, and pass it into each constructor (`NewConfucius`/`NewChongzhi`/`NewLiang`). Update the three constructors' signatures to accept and store it.
- [x] 2.3 Add an optional capability interface `RoundCapTracker { LastRunHitCap() bool }` in `internal/agent/agent.go` directly below `ToolCallTracker` (line ~59), with the same doc style: each agent sets a hit-cap flag at the end of `Run` if it exited via the cap path; callers must invoke `Run` before reading it.
- [x] 2.4 Add a `hitCapThisRun bool` field to each agent struct (guard with the existing `runMu` on Chongzhi, which already protects `executedToolThisRun`; plain field on Confucius/Liang which are single-Run-per-turn). Implement `LastRunHitCap() bool` on all three, and reset `hitCapThisRun = false` at the top of each `Run` (next to where Chongzhi resets `executedToolThisRun`, `chongzhi.go:82`).
- [x] 2.5 Extend `turnMeta` (`internal/agent/confucius.go:505`) with a `roundCapped bool` and an `observeRoundCapped()` setter under the existing mutex. Extend `snapshot()` (line 562) — or add an accessor — so `buildBreakdown` (line 508) can carry `roundCapped` into `TurnBreakdown`.
- [x] 2.6 Extend `TurnBreakdown` (`internal/agent/agent.go`) with a `RoundCapped bool` field; populate it in `buildBreakdown`. Extend `buildMetaObject` (`internal/agent/orchestrator.go:522`) to emit `"round_capped": true` ONLY when `b.RoundCapped` is true (omit the key otherwise so non-capped turns serialize identically to today).

## 3. Shared wrap-up round + signal helper

- [x] 3.1 Create `internal/agent/roundcap.go` with `func runWrapUpRound(ctx context.Context, client LLMClient, agentName string, hub *stream.Hub, req LLMRequest) (content string, usage Usage, err error)`. It calls `client.StreamChat` once with the caller-built `req` (which MUST already have `Tools` cleared), streaming `token`/`reasoning` events exactly like a normal round, and returns the accumulated assistant content + usage + error. No tool dispatch, no loop.
- [x] 3.2 In `internal/agent/roundcap.go`, add `func emitCapHitWarn(agentName string, cap, executed int)` — a structured `logger.L().Warn("agent round cap reached", zap.String("agent", agentName), zap.Int("max_rounds", cap), zap.Int("rounds_executed", executed))`. Called on every cap-hit, all three loops.
- [x] 3.3 In `internal/agent/roundcap.go`, add `func emitCapExhaustedError(ctx context.Context, hub *stream.Hub, agentName string)` — emits `stream.AgentErrorEvent(agentName, "round cap exhausted: wrap-up round produced no content", "round_cap_exhausted")` then `stream.AgentEndEvent(agentName)` via `hub.SendCtx`. Called only on the genuine empty-end path (Decision 4).

## 4. Confucius loop integration

- [x] 4.1 In `Confucius.Run` (`internal/agent/confucius.go:113`), change the loop guard from `maxConfuciusRounds` to `c.maxRounds`. Track whether the loop exited due to the cap: set a local `capped bool` when the body completes a dispatch round without `break` and `i+1 == c.maxRounds` (i.e. the `for` would not iterate again). Natural `break` exits leave `capped = false`.
- [x] 4.2 After the loop, if `capped`: call `emitCapHitWarn(c.Name(), c.maxRounds, c.maxRounds)`; set `tmeta.observeRoundCapped()`; build a wrap-up `LLMRequest` identical to a normal round but with `Tools` omitted (do NOT set `req.Tools` even when `c.toolsIsNotNil`), then call `runWrapUpRound`. Fold the returned usage into `total` and `byAgent[c.Name()]`.
- [x] 4.3 If the wrap-up round returns non-empty content → `finalContent = content`; leave `err == nil` (turn succeeds; `agent_error` is NOT emitted). If it returns empty content or an error → call `emitCapExhaustedError`; set the returned `finalContent` to whatever exists (likely `""`); for the error case, return `fmt.Errorf("confucius: round cap exhausted: %w", err)` so `orchestrator.Handle` surfaces `done.error` via its existing error path (`orchestrator.go:435`). For the empty-but-no-error case, emit `done` normally — `finalContent` is empty and `meta.round_capped=true`; do NOT synthesize a fake error (the empty content + `round_cap_exhausted` `agent_error` event already signals the failure to the frontend). Confirm against the spec scenario "Cap hit with empty wrap-up round surfaces error": the `agent_error` event is the contract; `done.error` is set only on the LLM-error sub-path.
- [x] 4.4 Propagate sub-agent caps: add a `subCapped bool` to `toolResult`; in `dispatchSubAgent` (`confucius.go:297`), after `sub.Run`, consult the optional `RoundCapTracker` interface — if the sub-agent implements it and `LastRunHitCap()` is true, set `toolResult.subCapped = true`. In `Confucius.Run`'s result-collection loop (`confucius.go:187`), call `tmeta.observeRoundCapped()` when `result.subCapped` is true (so a sub-agent hitting its cap marks the whole turn).

## 5. Chongzhi loop integration

- [x] 5.1 In `Chongzhi.Run` (`internal/agent/chongzhi.go:85`), change the guard to `c.maxRounds`; add the same `capped` detection as 4.1.
- [x] 5.2 After the loop, if `capped`: `emitCapHitWarn`; set `c.hitCapThisRun = true` (under `runMu`) so the parent's `RoundCapTracker` consult sees it; build a tool-disabled wrap-up `req` and call `runWrapUpRound`; fold usage into `total`.
- [x] 5.3 On non-empty wrap-up content → `finalContent = content`, return success. On empty/error → `emitCapExhaustedError` and return the error (LLM-error sub-path) so Confucius's dispatch sees the failure in the `tool_result` (and Confucius may retry/redirect per existing dispatch semantics — note `LastRunExecutedTool` idempotency still applies). Confirm `hitCapThisRun` is set before returning on every capped path.

## 6. Liang loop integration

- [x] 6.1 In `Liang.Run` (`internal/agent/liang.go:81`), change the guard to `l.maxRounds`; add the same `capped` detection.
- [x] 6.2 After the loop, if `capped`: `emitCapHitWarn`; set `l.hitCapThisRun = true`; build the wrap-up `req` with `Tools` omitted AND apply `l.terminalResponseFormat(round)` so the wrap-up round carries `response_format: json_schema` when `output_schema` is configured (the wrap-up IS the terminal round — spec scenario "Structured-output agent wrap-up round"). Call `runWrapUpRound`; fold usage into `total`.
- [x] 6.3 Same success/empty-error handling as 5.3 (`finalContent` on success; `emitCapExhaustedError` + return error on the LLM-error sub-path; `hitCapThisRun` set on every capped path).

## 7. Tests

- [x] 7.1 Config tests (`internal/config`): `DefaultAgentMaxRounds() == 100`; `max_rounds: 3` parses; unset → `0`; negative → `validate()` error.
- [x] 7.2 Per-agent loop tests (extend `confucius_test.go` / `chongzhi_test.go` / `liang_test.go` with a fake `LLMClient` that emits `tool_calls` for `maxRounds` consecutive rounds then a content round): assert (a) with a successful wrap-up, `Run` returns the wrap-up content with no error and `LastRunHitCap()` is true; (b) with an empty wrap-up round, the `agent_error` event (`round_cap_exhausted`) is emitted onto the hub; (c) the WARN log path is exercised (capture via a test logger or assert on the emitted events); (d) a natural `finish_reason=stop` before the cap leaves `LastRunHitCap()` false and emits no `round_cap_exhausted`.
- [x] 7.3 Liang structured-output wrap-up test: with `output_schema` set and a cap-hit, assert the wrap-up round's `LLMRequest` carries `response_format` (extend the existing `TestLiang_OutputSchema_*` style).
- [x] 7.4 Confucius sub-agent-cap propagation test: a fake sub-agent whose `LastRunHitCap()` is true causes the turn's `TurnBreakdown.RoundCapped == true` and `buildMetaObject` emits `round_capped: true`; a turn with no caps omits the key.
- [x] 7.5 Wrap-up-round tool-disabled test: assert the wrap-up `LLMRequest.Tools` is empty/nil for all three agents (the fake client records the request).
- [x] 7.6 Usage attribution test: the wrap-up round's tokens are folded into the agent's `by_agent` entry and the turn total (extend an existing usage-attribution test).

## 8. Docs & validation

- [x] 8.1 Add a commented example to `config.example.yaml`: under one agent (e.g. Chongzhi), `# max_rounds: 100  # tool-calling loop cap; unset → default 100`.
- [x] 8.2 Update `CLAUDE.md`: (a) replace "Round limits are hard-coded in the agent implementations" (line 191) with the configurable `max_rounds` behavior + default 100; (b) in the SSE section, note the additive `usage.meta.round_capped` key and the `round_cap_exhausted` `agent_error` code on genuine empty-end; (c) note the wrap-up round (tool-disabled) on cap-hit.
- [x] 8.3 Run `make test` (unit) and `go test ./test/integration/...` (real orchestrator + handlers) — confirm no dispatch/regression and that a capped turn no longer ends silently.
- [x] 8.4 Run `make lint` (`go vet ./...` exit 0; this change's files gofmt-clean).

## 9. Production fix: wrap-up steering + tool_calls handling

Root cause of the "max_rounds=2 → silent stop, no error" bug observed with glm-5.2: the wrap-up round stripped tools but (a) gave the model no signal to synthesize, and (b) accepted the model's tool-call preamble as the final answer.

- [x] 9.1 In `runWrapUpRound` (`internal/agent/roundcap.go`), append a steering instruction ("round limit reached, no more tools, answer now") to the messages so the model synthesizes instead of continuing its tool-use flow.
- [x] 9.2 In `runWrapUpRound`, treat a wrap-up response carrying `tool_calls` as no usable content (return `""`) so the caller routes to the `round_cap_exhausted` error path instead of silently returning the preamble as the final answer.
- [x] 9.3 Add `TestChongzhi_RoundCap_WrapUpReturnsToolCalls_SurfacesError`; update the spec (new scenario + amended requirement), design (Decision 7), and `CLAUDE.md`; `make test` + `make lint` green.
