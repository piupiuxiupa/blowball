## 1. Create the prompt package

- [x] 1.1 Create `internal/prompt/render.go` with `RenderInput`, `ToolInfo`, `SkillInfo`, and `RenderSystemPrompt`.
- [x] 1.2 Implement single `# Environment` section rendering using Go strings.
- [x] 1.3 Implement `## Built-in Tools`, `## MCP Tools` (grouped by server), and `## Skills` sections with conditional omission when empty.
- [x] 1.4 Run `go build ./internal/prompt` to ensure the new package compiles.

## 2. Add unit tests for the prompt package

- [x] 2.1 Create `internal/prompt/render_test.go` with a test for environment-only rendering.
- [x] 2.2 Add a test verifying tools are rendered under built-in and per-server MCP sections.
- [x] 2.3 Add a test verifying skills are rendered as an XML-style catalog with usage instruction.
- [x] 2.4 Add a test verifying empty tool/skill lists do not produce empty sections.
- [x] 2.5 Run `go test ./internal/prompt/...` and ensure all tests pass.

## 3. Refactor orchestrator to use the prompt package

- [x] 3.1 Update `orchestratorFactory.renderSystemPrompt` signature to accept `workspaceRoot`.
- [x] 3.2 Extract tool/skill collection into private helpers that produce `[]prompt.ToolInfo` and `[]prompt.SkillInfo`.
- [x] 3.3 Replace the existing environment/tools/skills string building with a single call to `prompt.RenderSystemPrompt`.
- [x] 3.4 Remove the `renderEnvironment` helper from `orchestrator.go`.
- [x] 3.5 Update `buildAgentRegistry` to pass `workspaceRoot` into `renderSystemPrompt`.

## 4. Remove duplicate environment injection from the client layer

- [x] 4.1 Remove the `AppendSystemPromptEnv` call in `internal/agent/openai_client.go:toOpenAIMessage`.
- [x] 4.2 Delete `internal/agent/prompts.go`.
- [x] 4.3 Verify `go build ./internal/agent/...` compiles.

## 5. Update tests

- [x] 5.1 Update `internal/agent/orchestrator_test.go` assertions to expect the new unified `# Environment` format.
- [x] 5.2 Update `internal/agent/openai_client_test.go` if it asserts on system message content.
- [x] 5.3 Run `go test ./internal/agent/... ./internal/prompt/...` and fix any failures.

## 6. Clean up obsolete context value

- [x] 6.1 Verify `ctx.Value("workspace")` is no longer used anywhere outside tests.
- [x] 6.2 Remove the `context.WithValue(ctx, "workspace", workspaceRoot)` line in `internal/handler/session.go` if it is unused.
- [x] 6.3 Run `go test ./internal/handler/...` and integration tests to confirm nothing breaks.

## 7. Final verification

- [x] 7.1 Run `go test ./...` (or the full project test suite) and ensure all tests pass.
- [x] 7.2 Run `go vet ./...` and fix any warnings.
- [x] 7.3 Review the final system prompt output manually or via a debug log to confirm only one `# Environment` section exists.
